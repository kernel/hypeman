//go:build linux

package diskutilization

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestCollect_CountsReflinkSnapshotExtentsOnce(t *testing.T) {
	p := paths.New(t.TempDir())

	srcPath := filepath.Join(p.InstanceSnapshotLatest("inst-1"), "memory-ranges")
	dstPath := filepath.Join(p.InstanceSnapshotLatest("inst-2"), "memory-ranges")
	require.NoError(t, os.MkdirAll(filepath.Dir(srcPath), 0755))
	require.NoError(t, os.MkdirAll(filepath.Dir(dstPath), 0755))
	require.NoError(t, os.WriteFile(srcPath, bytes.Repeat([]byte("m"), 4*1024*1024), 0644))

	cloned, err := cloneTestFile(srcPath, dstPath)
	require.NoError(t, err)
	if !cloned {
		t.Skip("FICLONE is not supported by this filesystem")
	}

	_, _, sawShared, err := fiemapAllocatedBytes(srcPath, newSharedExtentTracker())
	if err != nil {
		t.Skipf("FIEMAP is not supported by this filesystem: %v", err)
	}
	if !sawShared {
		t.Skip("FICLONE did not produce FIEMAP shared extents")
	}

	srcAllocated := allocatedBytesForPath(srcPath)
	dstAllocated := allocatedBytesForPath(dstPath)
	require.Greater(t, srcAllocated, int64(0))
	require.Equal(t, srcAllocated, dstAllocated)

	utilization, err := Collect(p)
	require.NoError(t, err)

	require.InDelta(t, srcAllocated, utilization.SnapshotShared, float64(64*1024))
	require.Less(t, utilization.SnapshotUncompressed, srcAllocated)
	require.Less(t, utilization.SnapshotUncompressed+utilization.SnapshotShared, srcAllocated+dstAllocated)
}

func TestSharedExtentTrackerAdd(t *testing.T) {
	tracker := newSharedExtentTracker()

	require.Equal(t, int64(100), tracker.add(1000, 100))
	require.Equal(t, int64(0), tracker.add(1000, 100))
	require.Equal(t, int64(50), tracker.add(1050, 100))
	require.Equal(t, int64(100), tracker.add(1200, 100))
	require.Equal(t, int64(100), tracker.add(900, 200))
}

func cloneTestFile(srcPath, dstPath string) (bool, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return false, err
	}
	defer src.Close()

	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return false, err
	}
	defer dst.Close()

	if err := unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())); err != nil {
		if errors.Is(err, unix.EINVAL) ||
			errors.Is(err, unix.ENOTSUP) ||
			errors.Is(err, unix.EOPNOTSUPP) ||
			errors.Is(err, unix.EXDEV) ||
			errors.Is(err, unix.ENOTTY) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
