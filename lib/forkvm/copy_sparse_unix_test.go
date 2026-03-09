//go:build darwin || linux

package forkvm

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestCopyGuestDirectory_PreservesSparseFiles(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")
	require.NoError(t, os.MkdirAll(src, 0755))

	srcOverlay := filepath.Join(src, "overlay.raw")
	const logicalSize = 256 * 1024 * 1024 // 256 MiB logical, tiny physical
	require.NoError(t, writeSparseFile(srcOverlay, logicalSize))

	require.NoError(t, CopyGuestDirectory(src, dst))

	dstOverlay := filepath.Join(dst, "overlay.raw")
	srcInfo, err := os.Stat(srcOverlay)
	require.NoError(t, err)
	dstInfo, err := os.Stat(dstOverlay)
	require.NoError(t, err)
	assert.Equal(t, srcInfo.Size(), dstInfo.Size(), "logical size must match")

	srcAllocated, err := allocatedBytes(srcOverlay)
	require.NoError(t, err)
	dstAllocated, err := allocatedBytes(dstOverlay)
	require.NoError(t, err)

	// Guard against dense copy inflation.
	assert.Less(t, dstAllocated, int64(logicalSize/10), "destination should remain sparse")
	// Allow modest filesystem allocation variance while preserving sparsity.
	assert.LessOrEqual(t, dstAllocated, srcAllocated+8*1024*1024)
}

func TestCopyGuestDirectory_FailsWhenSparseSeekingUnsupported(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")
	require.NoError(t, os.MkdirAll(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "metadata.json"), []byte(`{"id":"abc"}`), 0644))

	origSeekData := seekDataFn
	origSeekHole := seekHoleFn
	seekDataFn = func(fd int, offset int64) (int64, error) {
		return 0, unix.EINVAL
	}
	seekHoleFn = func(fd int, offset int64) (int64, error) {
		return 0, unix.EINVAL
	}
	defer func() {
		seekDataFn = origSeekData
		seekHoleFn = origSeekHole
	}()

	err := CopyGuestDirectory(src, dst)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSparseCopyUnsupported), "error should indicate sparse support is required")
}

func TestCopyGuestDirectory_SkipsSocketRuntimeArtifacts(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "forkvm-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "dst")
	require.NoError(t, os.MkdirAll(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "metadata.json"), []byte(`{"id":"abc"}`), 0644))

	socketPath := filepath.Join(src, fmt.Sprintf("vz-%d.sock", time.Now().UnixNano()))
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	require.NoError(t, CopyGuestDirectory(src, dst))

	assert.NoFileExists(t, filepath.Join(dst, filepath.Base(socketPath)))
	assert.FileExists(t, filepath.Join(dst, "metadata.json"))
}

func writeSparseFile(path string, logicalSize int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.WriteAt([]byte("begin"), 0); err != nil {
		return err
	}
	if _, err := f.WriteAt([]byte("middle"), logicalSize/2); err != nil {
		return err
	}
	if _, err := f.WriteAt([]byte("end"), logicalSize-4); err != nil {
		return err
	}
	return f.Truncate(logicalSize)
}

func allocatedBytes(path string) (int64, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, err
	}
	return st.Blocks * 512, nil
}
