package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureBuildkitRootAlreadyMounted(t *testing.T) {
	root := filepath.Join(t.TempDir(), "buildkit")

	mountCalled := false
	err := ensureBuildkitRoot(root,
		func(path string) (bool, error) {
			assert.Equal(t, root, path)
			return true, nil
		},
		func(path string) error {
			mountCalled = true
			return nil
		},
	)

	require.NoError(t, err)
	assert.False(t, mountCalled, "tmpfs mount must not be attempted when root is already a mountpoint")
}

func TestEnsureBuildkitRootMountsTmpfsWhenUnmounted(t *testing.T) {
	root := filepath.Join(t.TempDir(), "buildkit")

	var mountedPath string
	err := ensureBuildkitRoot(root,
		func(path string) (bool, error) { return false, nil },
		func(path string) error {
			mountedPath = path
			return nil
		},
	)

	require.NoError(t, err)
	assert.Equal(t, root, mountedPath)
	assert.DirExists(t, root)
}

func TestEnsureBuildkitRootPropagatesMountFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "buildkit")

	err := ensureBuildkitRoot(root,
		func(path string) (bool, error) { return false, nil },
		func(path string) error { return errors.New("mount failed") },
	)

	require.ErrorContains(t, err, "mount failed")
}

func TestBuildkitWorkerGCConfig(t *testing.T) {
	// No bound configured: no worker section, GC stays at BuildKit defaults.
	assert.Empty(t, buildkitWorkerGCConfig(0))
	assert.Empty(t, buildkitWorkerGCConfig(-1))

	// Bounded GC: enabled with a quoted human-readable keep threshold.
	// BuildKit parses gckeepstorage as a size string; a bare integer would
	// be interpreted as bytes.
	cfg := buildkitWorkerGCConfig(45 * 1024 * 1024 * 1024)
	assert.Contains(t, cfg, "[worker.oci]")
	assert.Contains(t, cfg, "gc = true")
	assert.Contains(t, cfg, `gckeepstorage = "46080MB"`)
}
