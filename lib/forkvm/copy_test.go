package forkvm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopyGuestDirectory(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")

	require.NoError(t, os.MkdirAll(filepath.Join(src, "logs"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(src, "snapshots", "snapshot-latest"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "metadata.json"), []byte(`{"id":"abc"}`), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "config.ext4"), []byte("config"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "overlay.raw"), []byte("overlay"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "logs", "app.log"), []byte("hello"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "snapshots", "snapshot-latest", "config.json"), []byte(`{}`), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "snapshots", "snapshot-latest", "memory-ranges.lz4.tmp"), []byte("partial"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "snapshots", "snapshot-latest", "memory-ranges.zst.tmp"), []byte("partial"), 0644))
	require.NoError(t, os.Symlink("metadata.json", filepath.Join(src, "meta-link")))

	require.NoError(t, CopyGuestDirectory(src, dst))

	assert.FileExists(t, filepath.Join(dst, "metadata.json"))
	assert.FileExists(t, filepath.Join(dst, "config.ext4"))
	assert.FileExists(t, filepath.Join(dst, "overlay.raw"))
	assert.FileExists(t, filepath.Join(dst, "snapshots", "snapshot-latest", "config.json"))
	assert.NoFileExists(t, filepath.Join(dst, "snapshots", "snapshot-latest", "memory-ranges.lz4.tmp"))
	assert.NoFileExists(t, filepath.Join(dst, "snapshots", "snapshot-latest", "memory-ranges.zst.tmp"))
	assert.NoFileExists(t, filepath.Join(dst, "logs", "app.log"))
	assert.FileExists(t, filepath.Join(dst, "meta-link"))

	_, err := os.Stat(filepath.Join(dst, "logs"))
	assert.Error(t, err)
	assert.True(t, os.IsNotExist(err))

	linkTarget, err := os.Readlink(filepath.Join(dst, "meta-link"))
	require.NoError(t, err)
	assert.Equal(t, "metadata.json", linkTarget)
}

func TestCopyGuestDirectoryWithOptionsSkipsRelativePaths(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")

	require.NoError(t, os.MkdirAll(filepath.Join(src, "snapshots", "snapshot-latest"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "snapshots", "snapshot-latest", "memory"), []byte("memory"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "snapshots", "snapshot-latest", "state"), []byte("state"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "overlay.raw"), []byte("overlay"), 0644))

	require.NoError(t, CopyGuestDirectoryWithOptions(src, dst, CopyOptions{
		SkipRelativePaths: map[string]struct{}{
			filepath.Join("snapshots", "snapshot-latest", "memory"): {},
		},
	}))

	assert.NoFileExists(t, filepath.Join(dst, "snapshots", "snapshot-latest", "memory"))
	assert.FileExists(t, filepath.Join(dst, "snapshots", "snapshot-latest", "state"))
	assert.FileExists(t, filepath.Join(dst, "overlay.raw"))
}

func TestCopyRegularFile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src", "memory")
	dst := filepath.Join(t.TempDir(), "dst", "snapshots", "snapshot-latest", "memory")

	require.NoError(t, os.MkdirAll(filepath.Dir(src), 0755))
	require.NoError(t, os.WriteFile(src, []byte("memory"), 0640))

	require.NoError(t, CopyRegularFile(src, dst))

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, []byte("memory"), got)
	info, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0640), info.Mode().Perm())
}

func TestCopyGuestDirectory_DoesNotSkipTmpSuffixedDirectories(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")

	tmpDir := filepath.Join(src, "snapshots", "snapshot-latest", "memory-ranges.lz4.tmp")
	require.NoError(t, os.MkdirAll(tmpDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "nested.txt"), []byte("nested"), 0644))

	require.NoError(t, CopyGuestDirectory(src, dst))

	assert.DirExists(t, filepath.Join(dst, "snapshots", "snapshot-latest", "memory-ranges.lz4.tmp"))
	assert.FileExists(t, filepath.Join(dst, "snapshots", "snapshot-latest", "memory-ranges.lz4.tmp", "nested.txt"))
}
