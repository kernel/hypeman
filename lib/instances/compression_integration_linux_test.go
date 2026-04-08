//go:build linux

package instances

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/resources"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/kernel/hypeman/lib/system"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type compressionIntegrationHarness struct {
	name             string
	hypervisor       hypervisor.Type
	setup            func(t *testing.T) (*manager, string)
	requirePrereqs   func(t *testing.T)
	waitHypervisorUp func(ctx context.Context, inst *Instance) error
}

const compressionGuestExecTimeout = 20 * time.Second

func TestCloudHypervisorStandbyRestoreCompressionScenarios(t *testing.T) {
	t.Parallel()
	requireStandbyRestoreCompressionManualRun(t)

	runStandbyRestoreCompressionScenarios(t, compressionIntegrationHarness{
		name:       "cloud-hypervisor",
		hypervisor: hypervisor.TypeCloudHypervisor,
		setup: func(t *testing.T) (*manager, string) {
			return setupCompressionTestManagerForHypervisor(t, hypervisor.TypeCloudHypervisor)
		},
		requirePrereqs: requireKVMAccess,
		waitHypervisorUp: func(ctx context.Context, inst *Instance) error {
			return waitForVMReady(ctx, inst.SocketPath, 10*time.Second)
		},
	})
}

func TestFirecrackerStandbyRestoreCompressionScenarios(t *testing.T) {
	t.Parallel()

	runStandbyRestoreCompressionScenarios(t, compressionIntegrationHarness{
		name:       "firecracker",
		hypervisor: hypervisor.TypeFirecracker,
		setup: func(t *testing.T) (*manager, string) {
			return setupCompressionTestManagerForHypervisor(t, hypervisor.TypeFirecracker)
		},
		requirePrereqs: requireFirecrackerIntegrationPrereqs,
	})
}

func TestQEMUStandbyRestoreCompressionScenarios(t *testing.T) {
	t.Parallel()

	runStandbyRestoreCompressionScenarios(t, compressionIntegrationHarness{
		name:       "qemu",
		hypervisor: hypervisor.TypeQEMU,
		setup: func(t *testing.T) (*manager, string) {
			return setupCompressionTestManagerForHypervisor(t, hypervisor.TypeQEMU)
		},
		requirePrereqs: requireQEMUUsable,
		waitHypervisorUp: func(ctx context.Context, inst *Instance) error {
			return waitForQEMUReady(ctx, inst.SocketPath, 10*time.Second)
		},
	})
}

func setupCompressionTestManagerForHypervisor(t *testing.T, hvType hypervisor.Type) (*manager, string) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "hmcmp-")
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = os.RemoveAll(tmpDir)
	})
	prepareIntegrationTestDataDir(t, tmpDir)

	cfg := &config.Config{
		DataDir: tmpDir,
		Network: legacyParallelTestNetworkConfig(testNetworkSeq.Add(1)),
	}

	p := paths.New(tmpDir)
	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	systemManager := system.NewManager(p)
	networkManager := network.NewManager(p, cfg, nil)
	deviceManager := devices.NewManager(p)
	volumeManager := volumes.NewManager(p, 0, nil)
	limits := ResourceLimits{
		MaxOverlaySize:       100 * 1024 * 1024 * 1024,
		MaxVcpusPerInstance:  0,
		MaxMemoryPerInstance: 0,
	}
	mgr := NewManager(p, imageManager, systemManager, networkManager, deviceManager, volumeManager, limits, hvType, SnapshotPolicy{}, nil, nil).(*manager)

	resourceMgr := resources.NewManager(cfg, p)
	resourceMgr.SetInstanceLister(mgr)
	resourceMgr.SetImageLister(imageManager)
	resourceMgr.SetVolumeLister(volumeManager)
	require.NoError(t, resourceMgr.Initialize(context.Background()))
	mgr.SetResourceValidator(resourceMgr)

	t.Cleanup(func() {
		cleanupOrphanedProcesses(t, mgr)
	})

	return mgr, tmpDir
}

func runStandbyRestoreCompressionScenarios(t *testing.T, harness compressionIntegrationHarness) {
	t.Helper()
	harness.requirePrereqs(t)

	mgr, tmpDir := harness.setup(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	createNginxImageAndWait(t, ctx, imageManager)

	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           fmt.Sprintf("compression-%s", harness.name),
		Image:          integrationTestImageRef(t, "docker.io/library/nginx:alpine"),
		Size:           1024 * 1024 * 1024,
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     harness.hypervisor,
	})
	require.NoError(t, err)

	deleted := false
	t.Cleanup(func() {
		if !deleted {
			_ = mgr.DeleteInstance(context.Background(), inst.Id)
		}
	})

	inst = waitForRunningAndExecReady(t, ctx, mgr, inst.Id, harness.waitHypervisorUp)

	inFlightCompression := &snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
		Level:     intPtr(3),
	}
	inst = runCompressionCycle(t, ctx, mgr, p, inst, harness.waitHypervisorUp, "in-flight-zstd-3", inFlightCompression, false)

	completedCases := []struct {
		name string
		cfg  *snapshotstore.SnapshotCompressionConfig
	}{
		{
			name: "zstd-1",
			cfg: &snapshotstore.SnapshotCompressionConfig{
				Enabled:   true,
				Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
				Level:     intPtr(1),
			},
		},
		{
			name: "lz4-0",
			cfg: &snapshotstore.SnapshotCompressionConfig{
				Enabled:   true,
				Algorithm: snapshotstore.SnapshotCompressionAlgorithmLz4,
				Level:     intPtr(0),
			},
		},
	}

	for _, tc := range completedCases {
		inst = runCompressionCycle(t, ctx, mgr, p, inst, harness.waitHypervisorUp, tc.name, tc.cfg, true)
	}

	require.NoError(t, mgr.DeleteInstance(ctx, inst.Id))
	deleted = true
}

func runCompressionCycle(
	t *testing.T,
	ctx context.Context,
	mgr *manager,
	p *paths.Paths,
	inst *Instance,
	waitHypervisorUp func(ctx context.Context, inst *Instance) error,
	label string,
	cfg *snapshotstore.SnapshotCompressionConfig,
	waitForCompression bool,
) *Instance {
	t.Helper()

	markerPath := fmt.Sprintf("/tmp/%s.txt", label)
	markerValue := fmt.Sprintf("%s-%d", label, time.Now().UnixNano())
	writeGuestMarker(t, ctx, inst, markerPath, markerValue)

	inst, err := mgr.StandbyInstance(ctx, inst.Id, StandbyInstanceRequest{
		Compression: cloneCompressionConfig(cfg),
	})
	require.NoError(t, err)
	require.Equal(t, StateStandby, inst.State)
	require.True(t, inst.HasSnapshot)

	snapshotDir := p.InstanceSnapshotLatest(inst.Id)
	jobKey := mgr.snapshotJobKeyForInstance(inst.Id)

	if waitForCompression {
		waitForCompressionJobCompletion(t, mgr, jobKey, 3*time.Minute)
		requireCompressedSnapshotFile(t, snapshotDir, effectiveCompressionForCycle(cfg))
	} else {
		waitForCompressionJobStart(t, mgr, jobKey, 15*time.Second)
	}

	inst, err = mgr.RestoreInstance(ctx, inst.Id)
	require.NoError(t, err)
	inst = waitForRunningAndExecReady(t, ctx, mgr, inst.Id, waitHypervisorUp)
	assertGuestMarker(t, ctx, inst, markerPath, markerValue)

	waitForCompressionJobCompletion(t, mgr, jobKey, 30*time.Second)
	return inst
}

func effectiveCompressionForCycle(cfg *snapshotstore.SnapshotCompressionConfig) snapshotstore.SnapshotCompressionConfig {
	if cfg != nil {
		normalized, err := normalizeCompressionConfig(cfg)
		if err != nil {
			panic(err)
		}
		return normalized
	}
	return snapshotstore.SnapshotCompressionConfig{Enabled: false}
}

func waitForRunningAndExecReady(t *testing.T, ctx context.Context, mgr *manager, instanceID string, waitHypervisorUp func(context.Context, *Instance) error) *Instance {
	t.Helper()

	inst, err := waitForInstanceState(ctx, mgr, instanceID, StateRunning, 30*time.Second)
	require.NoError(t, err)
	if waitHypervisorUp != nil {
		require.NoError(t, waitHypervisorUp(ctx, inst))
	}
	require.NoError(t, waitForExecAgent(ctx, mgr, instanceID, 30*time.Second))
	waitForGuestExecReady(t, ctx, inst)
	return inst
}

func waitForGuestExecReady(t *testing.T, ctx context.Context, inst *Instance) {
	t.Helper()

	require.Eventually(t, func() bool {
		execCtx, cancel := context.WithTimeout(ctx, integrationTestTimeout(5*time.Second))
		defer cancel()

		output, exitCode, err := execCommand(execCtx, inst, "true")
		return err == nil && exitCode == 0 && output == ""
	}, integrationTestTimeout(15*time.Second), 500*time.Millisecond, "guest exec should succeed after restore")
}

func writeGuestMarker(t *testing.T, ctx context.Context, inst *Instance, path string, value string) {
	t.Helper()
	output, exitCode, err := execCommandWithRetry(ctx, inst, compressionGuestExecTimeout, "sh", "-c", fmt.Sprintf("printf %q > %s && sync", value, path))
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, output)
}

func assertGuestMarker(t *testing.T, ctx context.Context, inst *Instance, path string, expected string) {
	t.Helper()
	output, exitCode, err := execCommandWithRetry(ctx, inst, compressionGuestExecTimeout, "cat", path)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, output)
	assert.Equal(t, expected, output)
}

func execCommandWithRetry(ctx context.Context, inst *Instance, timeout time.Duration, command ...string) (string, int, error) {
	deadline := time.Now().Add(integrationTestTimeout(timeout))
	var lastOutput string
	var lastExitCode int
	var lastErr error

	for {
		execCtx, cancel := context.WithTimeout(ctx, integrationTestTimeout(5*time.Second))
		output, exitCode, err := execCommand(execCtx, inst, command...)
		cancel()

		if err == nil {
			return output, exitCode, nil
		}

		lastOutput = output
		lastExitCode = exitCode
		lastErr = err

		if time.Now().After(deadline) {
			return lastOutput, lastExitCode, lastErr
		}

		time.Sleep(500 * time.Millisecond)
	}
}

func waitForCompressionJobStart(t *testing.T, mgr *manager, key string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mgr.compressionMu.Lock()
		job := mgr.compressionJobs[key]
		mgr.compressionMu.Unlock()
		if job != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("compression job %q did not start within %v", key, timeout)
}

func waitForCompressionJobCompletion(t *testing.T, mgr *manager, key string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	require.NoError(t, mgr.waitCompressionJobContext(ctx, key))
}

func requireCompressedSnapshotFile(t *testing.T, snapshotDir string, cfg snapshotstore.SnapshotCompressionConfig) {
	t.Helper()
	require.True(t, cfg.Enabled)

	rawPath, rawExists := findRawSnapshotMemoryFile(snapshotDir)
	assert.False(t, rawExists, "raw snapshot memory should be removed after compression, found %q", rawPath)

	compressedPath, algorithm, ok := findCompressedSnapshotMemoryFile(snapshotDir)
	require.True(t, ok, "compressed snapshot memory should exist in %s", snapshotDir)
	assert.Equal(t, cfg.Algorithm, algorithm)
	_, err := os.Stat(compressedPath)
	require.NoError(t, err)
}
