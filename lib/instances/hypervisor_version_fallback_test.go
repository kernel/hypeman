package instances

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveStartVersion(t *testing.T) {
	unsupported := errors.New("unsupported cloud-hypervisor version: v51.1")

	resolver := func(requested string) (string, error) {
		if requested == "" {
			return "v51.2", nil
		}
		if requested != "v51.2" {
			return "", unsupported
		}
		return requested, nil
	}

	t.Run("supported version is kept", func(t *testing.T) {
		version, fellBack, err := resolveStartVersion("v51.2", resolver)
		require.NoError(t, err)
		assert.Equal(t, "v51.2", version)
		assert.False(t, fellBack)
	})

	t.Run("removed version falls back to default", func(t *testing.T) {
		version, fellBack, err := resolveStartVersion("v51.1", resolver)
		require.NoError(t, err)
		assert.Equal(t, "v51.2", version)
		assert.True(t, fellBack)
	})

	t.Run("both resolutions failing returns the error", func(t *testing.T) {
		_, _, err := resolveStartVersion("v51.1", func(string) (string, error) {
			return "", unsupported
		})
		require.ErrorIs(t, err, unsupported)
	})
}

// versionFallbackStarter mimics a host where only the default version exists.
type versionFallbackStarter struct {
	startedVersion string
}

func (s *versionFallbackStarter) ValidateConfig(hypervisor.VMConfig) error { return nil }
func (s *versionFallbackStarter) SocketName() string                       { return "version-fallback.sock" }
func (s *versionFallbackStarter) GetBinaryPath(*paths.Paths, string) (string, error) {
	return "", nil
}
func (s *versionFallbackStarter) GetVersion(*paths.Paths) (string, error) { return "v51.2", nil }
func (s *versionFallbackStarter) ResolveVersion(_ *paths.Paths, requested string) (string, error) {
	if requested == "" || requested == "v51.2" {
		return "v51.2", nil
	}
	return "", fmt.Errorf("unsupported cloud-hypervisor version: %s", requested)
}
func (s *versionFallbackStarter) StartVM(_ context.Context, _ *paths.Paths, version, _ string, _ hypervisor.VMConfig) (int, hypervisor.Hypervisor, error) {
	s.startedVersion = version
	return 0, lifecycleNoopHypervisor{state: hypervisor.StateRunning}, nil
}
func (s *versionFallbackStarter) RestoreVM(context.Context, *paths.Paths, string, string, string) (int, hypervisor.Hypervisor, error) {
	return 0, nil, fmt.Errorf("restore must not be attempted")
}
func (s *versionFallbackStarter) PrepareFork(context.Context, hypervisor.ForkPrepareRequest) (hypervisor.ForkPrepareResult, error) {
	return hypervisor.ForkPrepareResult{}, nil
}

// TestStartAndBootVMFallsBackForRemovedVersion drives a cold start whose
// on-disk metadata pins a version the starter no longer supports.
func TestStartAndBootVMFallsBackForRemovedVersion(t *testing.T) {
	mgr, dataDir := setupTestManager(t)
	hvType := hypervisor.Type("version-fallback-test")
	starter := &versionFallbackStarter{}
	mgr.vmStarters[hvType] = starter

	id := "version-fallback"
	dir := filepath.Join(dataDir, "guests", id)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	seed := StoredMetadata{
		Id:                id,
		Name:              id,
		HypervisorType:    hvType,
		HypervisorVersion: "v51.1",
		KernelVersion:     "ch-v6.12.9",
		SocketPath:        filepath.Join(dir, "version-fallback.sock"),
	}
	raw, err := json.Marshal(&seed)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.json"), raw, 0o644))

	loaded, err := mgr.loadMetadata(id)
	require.NoError(t, err)
	require.Equal(t, "v51.1", loaded.HypervisorVersion)

	img := &images.Image{Name: "proof", Digest: "sha256:" + strings.Repeat("0", 64)}
	err = mgr.startAndBootVM(context.Background(), &loaded.StoredMetadata, img, nil, true)
	t.Logf("startAndBootVM: %v, StartVM version: %q, stored version: %q",
		err, starter.startedVersion, loaded.HypervisorVersion)
	require.NoError(t, err)
	assert.Equal(t, "v51.2", starter.startedVersion)
	assert.Equal(t, "v51.2", loaded.HypervisorVersion)
}
