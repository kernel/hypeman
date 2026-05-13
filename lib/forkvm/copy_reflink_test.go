package forkvm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCopyGuestDirectory_ReflinkFallback exercises the sparse-copy fallback
// path. The reflink fast path is fs-dependent and not portable across CI
// runners; this test forces it off and verifies copy correctness.
func TestCopyGuestDirectory_ReflinkFallback(t *testing.T) {
	SetReflinkDisabled(true)
	t.Cleanup(func() { SetReflinkDisabled(false) })

	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")

	require.NoError(t, os.MkdirAll(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "rootfs.ext4"), []byte("rootfs-bytes"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "config.json"), []byte(`{"x":1}`), 0644))

	require.NoError(t, CopyGuestDirectory(src, dst))

	got, err := os.ReadFile(filepath.Join(dst, "rootfs.ext4"))
	require.NoError(t, err)
	assert.Equal(t, "rootfs-bytes", string(got))

	got, err = os.ReadFile(filepath.Join(dst, "config.json"))
	require.NoError(t, err)
	assert.Equal(t, `{"x":1}`, string(got))
}

// TestCopyGuestDirectory_ReflinkAttempted verifies that with reflink enabled
// (the default), the copy still produces a correct destination on filesystems
// where FICLONE either succeeds or falls back transparently. This is the
// happy-path smoke test for the new fast path; on filesystems that don't
// support FICLONE the fallback handles correctness.
func TestCopyGuestDirectory_ReflinkAttempted(t *testing.T) {
	SetReflinkDisabled(false)

	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")

	require.NoError(t, os.MkdirAll(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "rootfs.ext4"), []byte("rootfs-bytes"), 0644))

	require.NoError(t, CopyGuestDirectory(src, dst))

	got, err := os.ReadFile(filepath.Join(dst, "rootfs.ext4"))
	require.NoError(t, err)
	assert.Equal(t, "rootfs-bytes", string(got))
}
