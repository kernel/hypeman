package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/go-units"
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

func TestEnsureBuildkitRootPropagatesMountpointCheckFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "buildkit")

	err := ensureBuildkitRoot(root,
		func(path string) (bool, error) { return false, errors.New("proc unavailable") },
		func(path string) error { return nil },
	)

	require.ErrorContains(t, err, "check mountpoint")
}

func TestMountsContain(t *testing.T) {
	mounts := strings.Join([]string{
		"overlay / overlay rw,relatime,lowerdir=/ro,upperdir=/rw/upper,workdir=/rw/work 0 0",
		"/dev/vdb /var/lib/buildkit ext4 rw,relatime 0 0",
		"tmpfs /run tmpfs rw,nosuid,nodev 0 0",
	}, "\n")

	assert.True(t, mountsContain(mounts, "/var/lib/buildkit"))
	assert.False(t, mountsContain(mounts, "/var/lib/buildkit2"), "prefix match must not count")
	assert.False(t, mountsContain(mounts, "/var"))
	assert.False(t, mountsContain("", "/var/lib/buildkit"))
}

func TestBuildkitWorkerGCConfig(t *testing.T) {
	// No bounds: no worker section, GC stays at BuildKit defaults (tmpfs path).
	assert.Empty(t, buildkitWorkerGCConfig(0, 0))
	assert.Empty(t, buildkitWorkerGCConfig(45*1024*1024*1024, 0))
	assert.Empty(t, buildkitWorkerGCConfig(0, 45*1024*1024*1024))

	cfg := buildkitWorkerGCConfig(5*1024*1024*1024, 45*1024*1024*1024)
	golden := "\n[worker.oci]\n" +
		"  gc = true\n" +
		"  [[worker.oci.gcpolicy]]\n" +
		"    reservedSpace = \"5120MB\"\n" +
		"    maxUsedSpace = \"46080MB\"\n" +
		"    all = true\n"
	assert.Equal(t, golden, cfg)
	assert.NotContains(t, cfg, "gckeepstorage", "deprecated gckeepstorage must not be emitted")
}

// TestBuildkitWorkerGCConfigSizeDecode is the regression test for the
// incorrectly-scaled size bug: BuildKit decodes gcpolicy sizes through
// units.RAMInBytes, so the emitted strings must decode back to the intended
// byte counts.
func TestBuildkitWorkerGCConfigSizeDecode(t *testing.T) {
	cfg := buildkitWorkerGCConfig(5*1024*1024*1024, 45*1024*1024*1024)

	for key, wantBytes := range map[string]int64{
		"reservedSpace": 5 * 1024 * 1024 * 1024,
		"maxUsedSpace":  45 * 1024 * 1024 * 1024,
	} {
		var size string
		_, err := fmt.Sscanf(strings.TrimSpace(strings.Split(strings.Split(cfg, key+" = ")[1], "\n")[0]), "%q", &size)
		require.NoError(t, err)
		decoded, err := units.RAMInBytes(size)
		require.NoError(t, err)
		assert.Equal(t, wantBytes, decoded, "%s must decode to the intended byte count", key)
	}
}
