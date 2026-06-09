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

// skipUnlessAPFS skips a clone test when the temp volume cannot serve a
// clonefile (i.e. not APFS). On CI we set HYPEMAN_REQUIRE_APFS_CLONE=1 so the
// clone path can't silently skip and report a false green on a non-APFS runner;
// there the unsupported signal becomes a hard failure instead.
func skipUnlessAPFS(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrReflinkUnsupported) {
		return
	}
	if os.Getenv("HYPEMAN_REQUIRE_APFS_CLONE") == "1" {
		t.Fatalf("clonefile reported unsupported but HYPEMAN_REQUIRE_APFS_CLONE=1: %v", err)
	}
	t.Skip("clonefile unsupported on this volume; not APFS")
}

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
	t.Cleanup(func() { _ = listener.Close() })

	// Probe the volume so a non-APFS runner skips (or fails under CI) before we
	// assert on clone output rather than silently exercising the sparse path.
	skipUnlessAPFS(t, copyRegularFileReflink(filepath.Join(src, "config.ext4"), filepath.Join(base, "probe"), 0600))

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
	skipUnlessAPFS(t, err)
	require.NoError(t, err)

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(want, got))

	info, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0644), info.Mode().Perm())
}

// TestCopyRegularFileReflink_WriteIsolation verifies the clone is copy-on-write:
// mutating the clone must not change the source. This is the headline correctness
// property of the fast path — a fork must never corrupt the instance it forked from.
func TestCopyRegularFileReflink_WriteIsolation(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "rootfs.ext4")
	dst := filepath.Join(dir, "fork.ext4")

	orig := bytes.Repeat([]byte("source-data"), 8192)
	require.NoError(t, os.WriteFile(src, orig, 0644))

	err := copyRegularFileReflink(src, dst, 0644)
	skipUnlessAPFS(t, err)
	require.NoError(t, err)

	// Diverge the clone by overwriting it entirely.
	require.NoError(t, os.WriteFile(dst, bytes.Repeat([]byte("forked-data!"), 8192), 0644))

	// The source must be byte-for-byte unchanged (copy-on-write).
	got, err := os.ReadFile(src)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(orig, got), "writing to the clone must not modify the source")
}

// TestCopyGuestDirectory_DarwinReflinkFallback drives the darwin-specific
// rejection path: when clonefile reports an unsupported error, copyRegularFile
// must route through isReflinkUnsupportedError to the sparse copy and still
// produce byte-identical output. CI volumes are always APFS, so this is the
// only coverage of the clonefile-rejected -> sparse transition on darwin.
func TestCopyGuestDirectory_DarwinReflinkFallback(t *testing.T) {
	orig := cloneFileFn
	cloneFileFn = func(src, dst string, flags int) error { return unix.EXDEV }
	t.Cleanup(func() { cloneFileFn = orig })
	SetReflinkDisabled(false)

	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")
	require.NoError(t, os.MkdirAll(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "rootfs.ext4"), []byte("rootfs-bytes"), 0644))
	require.NoError(t, writeSparseFile(filepath.Join(src, "overlay.raw"), 16*1024*1024))

	require.NoError(t, CopyGuestDirectory(src, dst))

	got, err := os.ReadFile(filepath.Join(dst, "rootfs.ext4"))
	require.NoError(t, err)
	assert.Equal(t, "rootfs-bytes", string(got))

	want, err := os.ReadFile(filepath.Join(src, "overlay.raw"))
	require.NoError(t, err)
	got, err = os.ReadFile(filepath.Join(dst, "overlay.raw"))
	require.NoError(t, err)
	assert.True(t, bytes.Equal(want, got), "sparse fallback output must match source")
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
		// EEXIST is owned by the pre-clone os.Remove; EINVAL/ENOTDIR are
		// programming/path errors on clonefile(2). None are capability signals,
		// so they propagate rather than trigger fallback.
		{"EEXIST", unix.EEXIST, false},
		{"EINVAL", unix.EINVAL, false},
		{"ENOTDIR", unix.ENOTDIR, false},
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
