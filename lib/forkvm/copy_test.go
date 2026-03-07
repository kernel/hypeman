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
	require.NoError(t, os.Symlink("metadata.json", filepath.Join(src, "meta-link")))

	require.NoError(t, CopyGuestDirectory(src, dst))

	assert.FileExists(t, filepath.Join(dst, "metadata.json"))
	assert.FileExists(t, filepath.Join(dst, "config.ext4"))
	assert.FileExists(t, filepath.Join(dst, "overlay.raw"))
	assert.FileExists(t, filepath.Join(dst, "snapshots", "snapshot-latest", "config.json"))
	assert.NoFileExists(t, filepath.Join(dst, "logs", "app.log"))
	assert.FileExists(t, filepath.Join(dst, "meta-link"))

	_, err := os.Stat(filepath.Join(dst, "logs"))
	assert.Error(t, err)
	assert.True(t, os.IsNotExist(err))

	linkTarget, err := os.Readlink(filepath.Join(dst, "meta-link"))
	require.NoError(t, err)
	assert.Equal(t, "metadata.json", linkTarget)
}
