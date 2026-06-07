//go:build darwin

package forkvm

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestCopyGuestDirectory_CloneCorrectness exercises the real clonefile(2) path
// over a guest-shaped tree: regular files must be byte-identical, perms must be
// preserved (the Chmod contract), symlinks copied, and runtime sockets skipped.
func TestCopyGuestDirectory_CloneCorrectness(t *testing.T) {
	SetReflinkDisabled(false)

	// Use a short base path so the unix socket below fits within the sockaddr
	// length limit; macOS t.TempDir() paths under /var/folders are too long.
	base, err := os.MkdirTemp("/tmp", "forkvm-clone-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "dst")
	require.NoError(t, os.MkdirAll(src, 0755))

	rootfs := bytes.Repeat([]byte("rootfs-"), 1<<20) // ~7 MiB dense payload
	require.NoError(t, os.WriteFile(filepath.Join(src, "rootfs.ext4"), rootfs, 0644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "config.ext4"), []byte("config-bytes"), 0600))
	require.NoError(t, writeSparseFile(filepath.Join(src, "overlay.raw"), 64*1024*1024))
	require.NoError(t, os.Symlink("rootfs.ext4", filepath.Join(src, "rootfs-link")))

	socketPath := filepath.Join(src, "vsock.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	require.NoError(t, CopyGuestDirectory(src, dst))

	for _, name := range []string{"rootfs.ext4", "config.ext4", "overlay.raw"} {
		want, err := os.ReadFile(filepath.Join(src, name))
		require.NoError(t, err)
		got, err := os.ReadFile(filepath.Join(dst, name))
		require.NoError(t, err)
		assert.True(t, bytes.Equal(want, got), "contents of %s must match", name)

		srcInfo, err := os.Stat(filepath.Join(src, name))
		require.NoError(t, err)
		dstInfo, err := os.Stat(filepath.Join(dst, name))
		require.NoError(t, err)
		assert.Equal(t, srcInfo.Mode().Perm(), dstInfo.Mode().Perm(), "perms of %s must be preserved", name)
	}

	target, err := os.Readlink(filepath.Join(dst, "rootfs-link"))
	require.NoError(t, err)
	assert.Equal(t, "rootfs.ext4", target)

	assert.NoFileExists(t, filepath.Join(dst, "vsock.sock"))
}

// TestCopyRegularFileReflink_StaleDestination guards the "destination must not
// exist" divergence from the Linux path: a pre-existing destination is removed
// before the clone, so the clone still succeeds and matches the source.
func TestCopyRegularFileReflink_StaleDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "rootfs.ext4")
	dst := filepath.Join(dir, "dst.ext4")

	want := bytes.Repeat([]byte("clone-me"), 4096)
	require.NoError(t, os.WriteFile(src, want, 0644))
	require.NoError(t, os.WriteFile(dst, []byte("stale-contents"), 0600))

	err := copyRegularFileReflink(src, dst, 0644)
	if errors.Is(err, ErrReflinkUnsupported) {
		t.Skip("clonefile unsupported on this volume; not APFS")
	}
	require.NoError(t, err)

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(want, got))

	info, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0644), info.Mode().Perm())
}

// TestCopyRegularFileReflink_SharesBlocks asserts the clone is copy-on-write:
// cloning a dense file consumes far less volume free space than the file's
// logical size because the clone shares the source's blocks. Skips when the
// temp volume is not APFS. Volume free space (statfs) is the reliable CoW
// signal here; a clone's per-file st_blocks still reports the full size on APFS.
func TestCopyRegularFileReflink_SharesBlocks(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "rootfs.ext4")
	dst := filepath.Join(dir, "clone.ext4")

	const size = 256 * 1024 * 1024 // 256 MiB dense
	require.NoError(t, os.WriteFile(src, bytes.Repeat([]byte("x"), size), 0644))

	if err := copyRegularFileReflink(src, dst, 0644); errors.Is(err, ErrReflinkUnsupported) {
		t.Skip("clonefile unsupported on this volume; not APFS")
	} else {
		require.NoError(t, err)
	}
	require.NoError(t, os.Remove(dst))

	freeBefore, err := freeBytes(dir)
	require.NoError(t, err)
	require.NoError(t, copyRegularFileReflink(src, dst, 0644))
	freeAfter, err := freeBytes(dir)
	require.NoError(t, err)

	consumed := freeBefore - freeAfter
	assert.Less(t, consumed, int64(size/2), "clone should share blocks, not consume the full logical size")
}

func freeBytes(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func TestIsReflinkUnsupportedError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"ENOTSUP", unix.ENOTSUP, true},
		{"EOPNOTSUPP", unix.EOPNOTSUPP, true},
		{"EXDEV", unix.EXDEV, true},
		{"EEXIST", unix.EEXIST, true},
		{"EINVAL", unix.EINVAL, true},
		{"ENOTDIR", unix.ENOTDIR, true},
		{"EIO", unix.EIO, false},
		{"ENOSPC", unix.ENOSPC, false},
		{"EACCES", unix.EACCES, false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isReflinkUnsupportedError(tc.err))
		})
	}
}
