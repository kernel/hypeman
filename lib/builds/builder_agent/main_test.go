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

	// Bounded GC: enabled with the keep threshold. gckeepstorage is integer
	// bytes, the portable form old buildkitd decodes; the explicit gcpolicy
	// carries both the retention floor (reservedSpace) and the reclaim
	// ceiling (maxUsedSpace) as unit strings (BuildKit >= v0.13).
	cfg := buildkitWorkerGCConfig(45 * 1024 * 1024 * 1024)
	assert.Contains(t, cfg, "[worker.oci]")
	assert.Contains(t, cfg, "gc = true")
	assert.Contains(t, cfg, "gckeepstorage = 48318382080")
	assert.Contains(t, cfg, "[[worker.oci.gcpolicy]]")
	assert.Contains(t, cfg, `reservedSpace = "46080MB"`)
	assert.Contains(t, cfg, `maxUsedSpace = "46080MB"`)
	assert.Contains(t, cfg, "all = true")
}
