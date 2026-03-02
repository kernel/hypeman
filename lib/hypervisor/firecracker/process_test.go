package firecracker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithSnapshotSourceDirAlias_RestoresSourceDirOnSuccess(t *testing.T) {
	tmp := t.TempDir()
	sourceDir := filepath.Join(tmp, "source")
	targetDir := filepath.Join(tmp, "target")

	require.NoError(t, os.MkdirAll(sourceDir, 0755))
	require.NoError(t, os.MkdirAll(targetDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "source-marker"), []byte("source"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(targetDir, "target-marker"), []byte("target"), 0644))

	err := withSnapshotSourceDirAlias(&restoreMetadata{SnapshotSourceDataDir: sourceDir}, targetDir, func() error {
		linkTarget, err := os.Readlink(sourceDir)
		require.NoError(t, err)
		assert.Equal(t, targetDir, linkTarget)
		return os.WriteFile(filepath.Join(sourceDir, "via-source-path"), []byte("ok"), 0644)
	})
	require.NoError(t, err)

	info, err := os.Lstat(sourceDir)
	require.NoError(t, err)
	assert.False(t, info.Mode()&os.ModeSymlink != 0, "source dir should be restored, not remain a symlink")

	_, err = os.Stat(filepath.Join(sourceDir, "source-marker"))
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(targetDir, "via-source-path"))
	require.NoError(t, err)
	assert.Equal(t, "ok", string(data))
}

func TestWithSnapshotSourceDirAlias_RestoresSourceDirOnRunError(t *testing.T) {
	tmp := t.TempDir()
	sourceDir := filepath.Join(tmp, "source")
	targetDir := filepath.Join(tmp, "target")

	require.NoError(t, os.MkdirAll(sourceDir, 0755))
	require.NoError(t, os.MkdirAll(targetDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "source-marker"), []byte("source"), 0644))

	expectedErr := errors.New("boom")
	err := withSnapshotSourceDirAlias(&restoreMetadata{SnapshotSourceDataDir: sourceDir}, targetDir, func() error {
		return expectedErr
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, expectedErr)

	info, statErr := os.Lstat(sourceDir)
	require.NoError(t, statErr)
	assert.False(t, info.Mode()&os.ModeSymlink != 0, "source dir should be restored, not remain a symlink")

	_, statErr = os.Stat(filepath.Join(sourceDir, "source-marker"))
	require.NoError(t, statErr)
}

func TestWithSnapshotSourceDirAlias_RejectsNestedPaths(t *testing.T) {
	tmp := t.TempDir()
	sourceDir := filepath.Join(tmp, "source")
	targetDir := filepath.Join(sourceDir, "fork")

	require.NoError(t, os.MkdirAll(sourceDir, 0755))
	require.NoError(t, os.MkdirAll(targetDir, 0755))

	err := withSnapshotSourceDirAlias(&restoreMetadata{SnapshotSourceDataDir: sourceDir}, targetDir, func() error {
		return nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not be nested")
}
