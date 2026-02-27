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
	require.NoError(t, os.WriteFile(filepath.Join(src, "metadata.json"), []byte(`{"id":"abc"}`), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "logs", "app.log"), []byte("hello"), 0644))
	require.NoError(t, os.Symlink("metadata.json", filepath.Join(src, "meta-link")))

	require.NoError(t, CopyGuestDirectory(src, dst))

	assert.FileExists(t, filepath.Join(dst, "metadata.json"))
	assert.FileExists(t, filepath.Join(dst, "logs", "app.log"))
	assert.FileExists(t, filepath.Join(dst, "meta-link"))

	app, err := os.ReadFile(filepath.Join(dst, "logs", "app.log"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(app))

	linkTarget, err := os.Readlink(filepath.Join(dst, "meta-link"))
	require.NoError(t, err)
	assert.Equal(t, "metadata.json", linkTarget)
}
