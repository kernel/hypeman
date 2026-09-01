package instances

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/healthcheck"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
	"github.com/kernel/hypeman/lib/paths"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	snapshottest "github.com/kernel/hypeman/lib/snapshot/testsupport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForkInstanceClearsVGPUAssignment(t *testing.T) {
	manager, _ := setupTestManager(t)
	ctx := context.Background()
	hvType := hypervisor.Type("fork-vgpu-test")
	hypervisor.RegisterCapabilities(hvType, hypervisor.Capabilities{SupportsConcurrentForkPrepare: true})
	manager.vmStarters[hvType] = concurrentForkPrepareTestStarter{}

	sourceID := "fork-vgpu-source"
	createStoppedSnapshotSourceFixture(t, manager, sourceID, sourceID, hvType)

	// A retained assignment (release failed during stop) must stay with the
	// source; the fork keeps only the profile and acquires its own vGPU on
	// start.
	meta, err := manager.loadMetadata(sourceID)
	require.NoError(t, err)
	meta.GPUProfile = "NVIDIA L40S-2Q"
	meta.GPUFramework = devices.VGPUFramework("future-framework")
	meta.GPUDevicePath = "/sys/bus/pci/devices/0000:82:00.4"
	meta.GPUMdevUUID = "retained-uuid"
	require.NoError(t, manager.saveMetadata(meta))

	forked, err := manager.ForkInstance(ctx, sourceID, ForkInstanceRequest{Name: "fork-vgpu-copy"})
	require.NoError(t, err)
	assert.Equal(t, "NVIDIA L40S-2Q", forked.GPUProfile)
	assert.Equal(t, devices.VGPUFrameworkNone, forked.GPUFramework)
	assert.Empty(t, forked.GPUDevicePath)
	assert.Empty(t, forked.GPUMdevUUID)

	source, err := manager.loadMetadata(sourceID)
	require.NoError(t, err)
	assert.Equal(t, "/sys/bus/pci/devices/0000:82:00.4", source.GPUDevicePath)
}

func TestForkInstance_VZStoppedSourceSupported(t *testing.T) {
	t.Parallel()
	manager, _ := setupTestManager(t)
	ctx := context.Background()
	if _, err := manager.getVMStarter(hypervisor.TypeVZ); err != nil {
		t.Skip("vz starter not available on this platform")
	}

	sourceID := "fork-vz-source"
	require.NoError(t, manager.ensureDirectories(sourceID))

	meta := &metadata{StoredMetadata: StoredMetadata{
		Id:                sourceID,
		Name:              "fork-vz-source",
		Image:             integrationTestImageRef(t, "docker.io/library/alpine:latest"),
		CreatedAt:         time.Now(),
		HypervisorType:    hypervisor.TypeVZ,
		HypervisorVersion: "test",
		SocketPath:        paths.New(manager.paths.DataDir()).InstanceSocket(sourceID, "vz.sock"),
		DataDir:           paths.New(manager.paths.DataDir()).InstanceDir(sourceID),
		VsockCID:          42,
		VsockSocket:       paths.New(manager.paths.DataDir()).InstanceVsockSocket(sourceID),
	}}
	require.NoError(t, manager.saveMetadata(meta))

	forked, err := manager.ForkInstance(ctx, sourceID, ForkInstanceRequest{Name: "fork-vz-copy"})
	require.NoError(t, err)
	require.NotNil(t, forked)
	assert.Equal(t, StateStopped, forked.State)
	assert.Equal(t, hypervisor.TypeVZ, forked.HypervisorType)
	assert.NotEqual(t, sourceID, forked.Id)
}

func TestResolveForkTargetState_DefaultsToSourceState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		source State
		want   State
	}{
		{name: "running", source: StateRunning, want: StateRunning},
		{name: "standby", source: StateStandby, want: StateStandby},
		{name: "stopped", source: StateStopped, want: StateStopped},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveForkTargetState("", tc.source)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestValidateForkRequest_InvalidTargetState(t *testing.T) {
	t.Parallel()
	err := validateForkRequest(ForkInstanceRequest{
		Name:        "fork-invalid-target",
		TargetState: State("Created"),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
}

func TestValidateForkVolumeSafety(t *testing.T) {
	t.Parallel()
	err := validateForkVolumeSafety([]VolumeAttachment{
		{VolumeID: "vol-rw", MountPath: "/data", Readonly: false},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotSupported)

	err = validateForkVolumeSafety([]VolumeAttachment{
		{VolumeID: "vol-ro", MountPath: "/data", Readonly: true, Overlay: false},
		{VolumeID: "vol-cow", MountPath: "/tmp", Readonly: true, Overlay: true},
	})
	require.NoError(t, err)
}

func TestCleanupForkInstanceOnError(t *testing.T) {
	t.Parallel()
	manager, _ := setupTestManager(t)
	ctx := context.Background()

	forkID := "fork-cleanup-target"
	require.NoError(t, manager.ensureDirectories(forkID))

	meta := &metadata{StoredMetadata: StoredMetadata{
		Id:                forkID,
		Name:              "fork-cleanup-target",
		Image:             integrationTestImageRef(t, "docker.io/library/alpine:latest"),
		CreatedAt:         time.Now(),
		HypervisorType:    hypervisor.TypeCloudHypervisor,
		HypervisorVersion: "test",
		SocketPath:        paths.New(manager.paths.DataDir()).InstanceSocket(forkID, "cloud-hypervisor.sock"),
		DataDir:           paths.New(manager.paths.DataDir()).InstanceDir(forkID),
		VsockCID:          43,
		VsockSocket:       paths.New(manager.paths.DataDir()).InstanceVsockSocket(forkID),
	}}
	require.NoError(t, manager.saveMetadata(meta))

	require.DirExists(t, meta.DataDir)
	require.NoError(t, manager.cleanupForkInstanceOnError(ctx, forkID))
	assert.NoDirExists(t, meta.DataDir)

	_, err := manager.loadMetadata(forkID)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestGetInstanceWaitsDuringSnapshotSourceAlias(t *testing.T) {
	manager, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "fork-alias-source"
	aliasID := "fork-alias-child"
	now := time.Now()
	for _, meta := range []*metadata{
		{StoredMetadata: StoredMetadata{
			Id:                sourceID,
			Name:              sourceID,
			Image:             integrationTestImageRef(t, "docker.io/library/alpine:latest"),
			CreatedAt:         now,
			StoppedAt:         &now,
			HypervisorType:    hypervisor.TypeFirecracker,
			HypervisorVersion: "test",
			SocketPath:        paths.New(manager.paths.DataDir()).InstanceSocket(sourceID, "fc.sock"),
			DataDir:           paths.New(manager.paths.DataDir()).InstanceDir(sourceID),
			VsockCID:          42,
			VsockSocket:       paths.New(manager.paths.DataDir()).InstanceVsockSocket(sourceID),
		}},
		{StoredMetadata: StoredMetadata{
			Id:                aliasID,
			Name:              aliasID,
			Image:             integrationTestImageRef(t, "docker.io/library/alpine:latest"),
			CreatedAt:         now,
			HypervisorType:    hypervisor.TypeFirecracker,
			HypervisorVersion: "test",
			SocketPath:        paths.New(manager.paths.DataDir()).InstanceSocket(aliasID, "fc.sock"),
			DataDir:           paths.New(manager.paths.DataDir()).InstanceDir(aliasID),
			VsockCID:          43,
			VsockSocket:       paths.New(manager.paths.DataDir()).InstanceVsockSocket(aliasID),
		}},
	} {
		require.NoError(t, manager.ensureDirectories(meta.Id))
		require.NoError(t, manager.saveMetadata(meta))
	}

	sourceDir := manager.paths.InstanceDir(sourceID)
	childDir := manager.paths.InstanceDir(aliasID)
	backupDir := sourceDir + ".bak"

	unlockAlias := hypervisor.LockSnapshotSourceAliasMutation()
	aliasLocked := true
	defer func() {
		if aliasLocked {
			unlockAlias()
		}
	}()
	require.NoError(t, os.Rename(sourceDir, backupDir))
	require.NoError(t, os.Symlink(childDir, sourceDir))

	type getResult struct {
		inst *Instance
		err  error
	}
	done := make(chan getResult, 1)
	go func() {
		inst, err := manager.GetInstance(ctx, sourceID)
		done <- getResult{inst: inst, err: err}
	}()

	select {
	case got := <-done:
		t.Fatalf("GetInstance returned during source alias mutation: inst=%v err=%v", got.inst, got.err)
	case <-time.After(25 * time.Millisecond):
	}

	require.NoError(t, os.Remove(sourceDir))
	require.NoError(t, os.Rename(backupDir, sourceDir))
	unlockAlias()
	aliasLocked = false

	got := <-done
	require.NoError(t, got.err)
	require.NotNil(t, got.inst)
	assert.Equal(t, sourceID, got.inst.Id)
}

func TestForkInstanceStoppedSourceUsesReadLock(t *testing.T) {
	manager, _ := setupTestManager(t)
	ctx := context.Background()
	hvType := hypervisor.Type("concurrent-fork-test")
	hypervisor.RegisterRuntime(hvType, hypervisor.RuntimeRegistration{
		Capabilities: func() hypervisor.Capabilities {
			return hypervisor.Capabilities{SupportsConcurrentForkPrepare: true}
		},
	})
	manager.vmStarters[hvType] = concurrentForkPrepareTestStarter{}

	sourceID := "fork-stopped-read-lock-source"
	createStoppedSnapshotSourceFixture(t, manager, sourceID, sourceID, hvType)

	sourceLock := manager.getInstanceLock(sourceID)
	sourceLock.RLock()
	defer sourceLock.RUnlock()

	type forkResult struct {
		inst *Instance
		err  error
	}
	done := make(chan forkResult, 1)
	go func() {
		inst, err := manager.ForkInstance(ctx, sourceID, ForkInstanceRequest{Name: "fork-stopped-read-lock-copy"})
		done <- forkResult{inst: inst, err: err}
	}()

	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.NotNil(t, got.inst)
		assert.Equal(t, StateStopped, got.inst.State)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("ForkInstance blocked behind a source read lock")
	}
}

type concurrentForkPrepareTestStarter struct{}

func (concurrentForkPrepareTestStarter) ValidateConfig(hypervisor.VMConfig) error { return nil }
func (concurrentForkPrepareTestStarter) SocketName() string                       { return "concurrent-fork-test.sock" }

func (concurrentForkPrepareTestStarter) GetBinaryPath(*paths.Paths, string) (string, error) {
	return "", nil
}

func (concurrentForkPrepareTestStarter) GetVersion(*paths.Paths) (string, error) { return "test", nil }
func (concurrentForkPrepareTestStarter) ResolveVersion(*paths.Paths, string) (string, error) {
	return "test", nil
}

func (concurrentForkPrepareTestStarter) StartVM(context.Context, *paths.Paths, string, string, hypervisor.VMConfig) (int, hypervisor.Hypervisor, error) {
	return 0, nil, nil
}

func (concurrentForkPrepareTestStarter) RestoreVM(_ context.Context, _ *paths.Paths, _ string, socketPath string, _ string, _ hypervisor.RestoreOptions) (int, hypervisor.Hypervisor, error) {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		return 0, nil, err
	}
	if err := os.WriteFile(socketPath, []byte("socket"), 0644); err != nil {
		return 0, nil, err
	}
	return 1, lifecycleNoopHypervisor{state: hypervisor.StateRunning}, nil
}

func (concurrentForkPrepareTestStarter) PrepareFork(context.Context, hypervisor.ForkPrepareRequest) (hypervisor.ForkPrepareResult, error) {
	return hypervisor.ForkPrepareResult{}, nil
}

func TestForkInstanceStandbyRunningTargetSkipsSourceWriteLockWithoutAlias(t *testing.T) {
	manager, _ := setupTestManager(t)
	ctx := context.Background()
	hvType := hypervisor.Type("concurrent-fork-restore-test")
	hypervisor.RegisterRuntime(hvType, hypervisor.RuntimeRegistration{
		Capabilities: func() hypervisor.Capabilities {
			return hypervisor.Capabilities{SupportsConcurrentForkPrepare: true}
		},
	})
	hypervisor.RegisterClientFactory(hvType, func(string) (hypervisor.Hypervisor, error) {
		return lifecycleNoopHypervisor{state: hypervisor.StateRunning}, nil
	})
	manager.vmStarters[hvType] = concurrentForkPrepareTestStarter{}

	sourceID := "fork-standby-running-read-lock-source"
	createStandbySnapshotSourceFixture(t, manager, sourceID, sourceID, hvType)
	meta, err := manager.loadMetadata(sourceID)
	require.NoError(t, err)
	meta.StoredMetadata.SkipGuestAgent = true
	require.NoError(t, manager.saveMetadata(meta))

	sourceLock := manager.getInstanceLock(sourceID)
	sourceLock.RLock()
	defer sourceLock.RUnlock()

	type forkResult struct {
		inst *Instance
		err  error
	}
	done := make(chan forkResult, 1)
	go func() {
		inst, err := manager.ForkInstance(ctx, sourceID, ForkInstanceRequest{
			Name:        "fork-standby-running-read-lock-copy",
			TargetState: StateRunning,
		})
		done <- forkResult{inst: inst, err: err}
	}()

	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.NotNil(t, got.inst)
		assert.Contains(t, []State{StateRunning, StateInitializing}, got.inst.State)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("ForkInstance blocked behind the source write lock for a no-alias restore")
	}
}

func TestApplyForkTargetStateWithSourceLockRequiresStandbySource(t *testing.T) {
	t.Parallel()

	manager, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "fork-alias-running-source"
	forkID := "fork-alias-running-child"
	now := time.Now()
	require.NoError(t, manager.ensureDirectories(sourceID))
	require.NoError(t, manager.ensureDirectories(forkID))

	sourceSocket := manager.paths.InstanceSocket(sourceID, "fc.sock")
	require.NoError(t, os.MkdirAll(filepath.Dir(sourceSocket), 0755))
	require.NoError(t, os.WriteFile(sourceSocket, []byte("socket"), 0644))
	manager.storeCachedHypervisorState(sourceID, hypervisor.StateRunning)
	require.NoError(t, manager.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:                sourceID,
		Name:              sourceID,
		Image:             integrationTestImageRef(t, "docker.io/library/alpine:latest"),
		CreatedAt:         now,
		HypervisorType:    hypervisor.TypeFirecracker,
		HypervisorVersion: "test",
		SocketPath:        sourceSocket,
		DataDir:           manager.paths.InstanceDir(sourceID),
		VsockCID:          42,
		VsockSocket:       manager.paths.InstanceVsockSocket(sourceID),
	}}))

	forkSnapshotDir := manager.paths.InstanceSnapshotLatest(forkID)
	require.NoError(t, os.MkdirAll(forkSnapshotDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(forkSnapshotDir, "memory"), []byte("snapshot"), 0644))
	require.NoError(t, manager.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:                forkID,
		Name:              forkID,
		Image:             integrationTestImageRef(t, "docker.io/library/alpine:latest"),
		CreatedAt:         now,
		HypervisorType:    hypervisor.TypeFirecracker,
		HypervisorVersion: "test",
		SocketPath:        manager.paths.InstanceSocket(forkID, "fc.sock"),
		DataDir:           manager.paths.InstanceDir(forkID),
		VsockCID:          42,
		VsockSocket:       manager.paths.InstanceVsockSocket(forkID),
		SkipGuestAgent:    true,
	}}))

	_, err := manager.applyForkTargetStateWithSourceLock(ctx, manager.getInstanceLock(sourceID), sourceID, forkID, StateRunning)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidState)
	assert.Contains(t, err.Error(), "source fork-alias-running-source is now")
}

func TestForkInstanceStoppedSourceRequiresWriteLockWithoutConcurrentPrepare(t *testing.T) {
	manager, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "fork-stopped-write-lock-source"
	createStoppedSnapshotSourceFixture(t, manager, sourceID, sourceID, hypervisor.TypeQEMU)

	sourceLock := manager.getInstanceLock(sourceID)
	sourceLock.RLock()

	type forkResult struct {
		inst *Instance
		err  error
	}
	done := make(chan forkResult, 1)
	go func() {
		inst, err := manager.ForkInstance(ctx, sourceID, ForkInstanceRequest{Name: "fork-stopped-write-lock-copy"})
		done <- forkResult{inst: inst, err: err}
	}()

	select {
	case got := <-done:
		sourceLock.RUnlock()
		t.Fatalf("ForkInstance returned without exclusive source lock: inst=%v err=%v", got.inst, got.err)
	case <-time.After(25 * time.Millisecond):
	}

	sourceLock.RUnlock()
	got := <-done
	require.NoError(t, got.err)
	require.NotNil(t, got.inst)
	assert.Equal(t, StateStopped, got.inst.State)
}

func TestForkInstance_CleansUpOnTargetTransitionError(t *testing.T) {
	t.Parallel()
	manager, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "fork-target-transition-source"
	require.NoError(t, manager.ensureDirectories(sourceID))

	now := time.Now()
	meta := &metadata{StoredMetadata: StoredMetadata{
		Id:                sourceID,
		Name:              sourceID,
		Image:             "docker.io/library/nonexistent:latest",
		CreatedAt:         now,
		StoppedAt:         &now,
		HypervisorType:    hypervisor.TypeCloudHypervisor,
		HypervisorVersion: "test",
		SocketPath:        paths.New(manager.paths.DataDir()).InstanceSocket(sourceID, "cloud-hypervisor.sock"),
		DataDir:           paths.New(manager.paths.DataDir()).InstanceDir(sourceID),
		VsockCID:          42,
		VsockSocket:       paths.New(manager.paths.DataDir()).InstanceVsockSocket(sourceID),
	}}
	require.NoError(t, manager.saveMetadata(meta))

	_, err := manager.ForkInstance(ctx, sourceID, ForkInstanceRequest{
		Name:        "fork-target-transition-copy",
		TargetState: StateRunning,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "apply fork target state")

	entries, err := os.ReadDir(manager.paths.GuestsDir())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, sourceID, entries[0].Name())
}

func TestForkInstanceRejectsDuplicateNameForNonNetworkedSource(t *testing.T) {
	t.Parallel()
	manager, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "fork-duplicate-name-source"
	require.NoError(t, manager.ensureDirectories(sourceID))

	now := time.Now()
	sourceMeta := &metadata{StoredMetadata: StoredMetadata{
		Id:                sourceID,
		Name:              sourceID,
		Image:             integrationTestImageRef(t, "docker.io/library/alpine:latest"),
		CreatedAt:         now,
		StoppedAt:         &now,
		HypervisorType:    hypervisor.TypeCloudHypervisor,
		HypervisorVersion: "test",
		SocketPath:        paths.New(manager.paths.DataDir()).InstanceSocket(sourceID, "cloud-hypervisor.sock"),
		DataDir:           paths.New(manager.paths.DataDir()).InstanceDir(sourceID),
		VsockCID:          42,
		VsockSocket:       paths.New(manager.paths.DataDir()).InstanceVsockSocket(sourceID),
	}}
	require.NoError(t, manager.saveMetadata(sourceMeta))

	existingID := "fork-duplicate-name-existing"
	require.NoError(t, manager.ensureDirectories(existingID))
	existingMeta := &metadata{StoredMetadata: StoredMetadata{
		Id:                existingID,
		Name:              "duplicate-name",
		Image:             integrationTestImageRef(t, "docker.io/library/alpine:latest"),
		CreatedAt:         now,
		StoppedAt:         &now,
		HypervisorType:    hypervisor.TypeCloudHypervisor,
		HypervisorVersion: "test",
		SocketPath:        paths.New(manager.paths.DataDir()).InstanceSocket(existingID, "cloud-hypervisor.sock"),
		DataDir:           paths.New(manager.paths.DataDir()).InstanceDir(existingID),
		VsockCID:          43,
		VsockSocket:       paths.New(manager.paths.DataDir()).InstanceVsockSocket(existingID),
	}}
	require.NoError(t, manager.saveMetadata(existingMeta))

	_, err := manager.ForkInstance(ctx, sourceID, ForkInstanceRequest{Name: "duplicate-name"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAlreadyExists)
	assert.Contains(t, err.Error(), "already exists")
}

func TestSaveForkMetadataSerializesNameAdmission(t *testing.T) {
	t.Parallel()
	manager, _ := setupTestManager(t)
	ctx := context.Background()

	now := time.Now()
	metas := []*metadata{
		{StoredMetadata: StoredMetadata{
			Id:                "fork-concurrent-name-a",
			Name:              "fork-concurrent-name",
			Image:             integrationTestImageRef(t, "docker.io/library/alpine:latest"),
			CreatedAt:         now,
			HypervisorType:    hypervisor.TypeCloudHypervisor,
			HypervisorVersion: "test",
			SocketPath:        paths.New(manager.paths.DataDir()).InstanceSocket("fork-concurrent-name-a", "cloud-hypervisor.sock"),
			DataDir:           paths.New(manager.paths.DataDir()).InstanceDir("fork-concurrent-name-a"),
			VsockCID:          42,
			VsockSocket:       paths.New(manager.paths.DataDir()).InstanceVsockSocket("fork-concurrent-name-a"),
		}},
		{StoredMetadata: StoredMetadata{
			Id:                "fork-concurrent-name-b",
			Name:              "fork-concurrent-name",
			Image:             integrationTestImageRef(t, "docker.io/library/alpine:latest"),
			CreatedAt:         now,
			HypervisorType:    hypervisor.TypeCloudHypervisor,
			HypervisorVersion: "test",
			SocketPath:        paths.New(manager.paths.DataDir()).InstanceSocket("fork-concurrent-name-b", "cloud-hypervisor.sock"),
			DataDir:           paths.New(manager.paths.DataDir()).InstanceDir("fork-concurrent-name-b"),
			VsockCID:          43,
			VsockSocket:       paths.New(manager.paths.DataDir()).InstanceVsockSocket("fork-concurrent-name-b"),
		}},
	}
	for _, meta := range metas {
		require.NoError(t, manager.ensureDirectories(meta.Id))
	}

	var wg sync.WaitGroup
	errs := make([]error, len(metas))
	for i := range metas {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = manager.saveForkMetadata(ctx, metas[i])
		}(i)
	}
	wg.Wait()

	successes := 0
	duplicates := 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrAlreadyExists):
			duplicates++
		default:
			require.NoError(t, err)
		}
	}
	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, duplicates)
}

func TestForkInstanceFromStandbyCancelsCompressionJobAndCopiesRawMemory(t *testing.T) {
	t.Parallel()

	manager, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "fork-standby-compressed-src"
	createStandbySnapshotSourceFixture(t, manager, sourceID, sourceID, manager.defaultHypervisor)
	expiresAt := time.Now().Add(time.Hour)
	sourceMeta, err := manager.loadMetadata(sourceID)
	require.NoError(t, err)
	sourceMeta.ExpiresAt = &expiresAt
	require.NoError(t, manager.saveMetadata(sourceMeta))

	rawPath := filepath.Join(manager.paths.InstanceSnapshotLatest(sourceID), "memory-ranges")
	require.NoError(t, os.WriteFile(rawPath, []byte("some guest memory"), 0o644))
	snapshotConfigPath := manager.paths.InstanceSnapshotConfig(sourceID)
	require.NoError(t, os.MkdirAll(filepath.Dir(snapshotConfigPath), 0o755))
	require.NoError(t, os.WriteFile(snapshotConfigPath, []byte(`{}`), 0o644))

	_, _, err = compressSnapshotMemoryFile(ctx, rawPath, snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
		Level:     intPtr(1),
	})
	require.NoError(t, err)

	var canceled atomic.Bool
	done := make(chan struct{})

	manager.compressionMu.Lock()
	manager.compressionJobs[manager.snapshotJobKeyForInstance(sourceID)] = &compressionJob{
		cancel: func() {
			canceled.Store(true)
			select {
			case <-done:
			default:
				close(done)
			}
		},
		done: done,
		target: compressionTarget{
			Key:         manager.snapshotJobKeyForInstance(sourceID),
			OwnerID:     sourceID,
			SnapshotDir: manager.paths.InstanceSnapshotLatest(sourceID),
		},
	}
	manager.compressionMu.Unlock()

	forked, _, err := manager.forkInstanceFromStoppedOrStandby(ctx, sourceID, ForkInstanceRequest{
		Name:        "fork-standby-compressed-copy",
		TargetState: StateStopped,
	}, true)
	require.NoError(t, err)
	require.NotNil(t, forked)
	assert.Nil(t, forked.ExpiresAt)

	assert.True(t, canceled.Load(), "standby compression job should be canceled before copying the source guest directory")

	forkSnapshotDir := manager.paths.InstanceSnapshotLatest(forked.Id)
	_, ok := findRawSnapshotMemoryFile(forkSnapshotDir)
	assert.True(t, ok, "forked standby guest directory should contain raw memory after preparing the source snapshot")
	_, _, ok = findCompressedSnapshotMemoryFile(forkSnapshotDir)
	assert.False(t, ok, "forked standby guest directory should not retain compressed memory artifacts from the source instance")
}

func TestApplyForkTargetStateStoppedRefreshesSnapshotForkCID(t *testing.T) {
	t.Parallel()

	manager, _ := setupTestManager(t)
	ctx := context.Background()

	forkID := "fork-target-stopped"
	require.NoError(t, manager.ensureDirectories(forkID))
	snapshotDir := manager.paths.InstanceSnapshotLatest(forkID)
	require.NoError(t, os.MkdirAll(snapshotDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(snapshotDir, "memory"), []byte("snapshot"), 0644))

	const sourceCID = int64(100)
	require.NotEqual(t, sourceCID, generateVsockCID(forkID))
	meta := &metadata{StoredMetadata: StoredMetadata{
		Id:             forkID,
		Name:           forkID,
		CreatedAt:      time.Now(),
		HypervisorType: hypervisor.TypeFirecracker,
		SocketPath:     manager.paths.InstanceSocket(forkID, "firecracker.sock"),
		DataDir:        manager.paths.InstanceDir(forkID),
		VsockCID:       sourceCID,
		VsockSocket:    manager.paths.InstanceVsockSocket(forkID),
	}}
	meta.StoredMetadata.Phases.Record(phasetracking.PhaseStandby, time.Now())
	require.NoError(t, manager.saveMetadata(meta))

	inst, err := manager.applyForkTargetState(ctx, forkID, StateStopped)
	require.NoError(t, err)
	require.Equal(t, StateStopped, inst.State)
	require.Equal(t, generateVsockCID(forkID), inst.VsockCID)
	require.NoDirExists(t, snapshotDir)

	updated, err := manager.loadMetadata(forkID)
	require.NoError(t, err)
	assert.Equal(t, generateVsockCID(forkID), updated.StoredMetadata.VsockCID)
}

func TestCloneStoredMetadataForFork_DeepCopiesReferenceFields(t *testing.T) {
	t.Parallel()
	startedAt := time.Now().Add(-2 * time.Minute)
	stoppedAt := time.Now().Add(-1 * time.Minute)
	expiresAt := time.Now().Add(time.Hour)
	notBefore := time.Now().Add(5 * time.Minute)
	pid := 1234
	exitCode := 17
	compressionLevel := 5
	pendingLevel := 3

	src := StoredMetadata{
		Image:                     "docker.io/library/alpine:3.19",
		ResolvedImage:             "docker.io/library/alpine@sha256:amd64digest",
		Platform:                  "linux/amd64",
		Env:                       map[string]string{"A": "1"},
		Tags:                      map[string]string{"m": "x"},
		Volumes:                   []VolumeAttachment{{VolumeID: "vol-1", MountPath: "/data"}},
		Devices:                   []string{"0000:01:00.0"},
		Entrypoint:                []string{"/bin/sh", "-c"},
		Cmd:                       []string{"echo", "hello"},
		ExpiresAt:                 &expiresAt,
		StartedAt:                 &startedAt,
		StoppedAt:                 &stoppedAt,
		HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &pid},
		ExitCode:                  &exitCode,
		AutoStandby: &autostandby.Policy{
			Enabled:                true,
			IdleTimeout:            "5m",
			IgnoreSourceCIDRs:      []string{"10.0.0.0/8"},
			IgnoreDestinationPorts: []uint16{22},
		},
		HealthCheck: &healthcheck.Policy{
			Type: healthcheck.TypeHTTP,
			HTTP: &healthcheck.HTTPCheck{
				Port: 80,
				Path: "/",
			},
		},
		SnapshotPolicy: &SnapshotPolicy{
			Compression: &snapshotstore.SnapshotCompressionConfig{
				Enabled:   true,
				Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
				Level:     &compressionLevel,
			},
		},
		PendingStandbyCompression: &PendingStandbyCompression{
			Policy: snapshotstore.SnapshotCompressionConfig{
				Enabled:   true,
				Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
				Level:     &pendingLevel,
			},
			NotBefore: notBefore,
		},
	}

	cloned := cloneStoredMetadata(src)
	require.Equal(t, src, cloned)
	require.Equal(t, "linux/amd64", cloned.Platform, "resolved platform must survive fork/snapshot-restore")
	require.Equal(t, "docker.io/library/alpine@sha256:amd64digest", cloned.ResolvedImage, "resolved boot image must survive fork/snapshot-restore")

	cloned.Env["A"] = "2"
	cloned.Tags["m"] = "y"
	cloned.Volumes[0].MountPath = "/mnt"
	cloned.Devices[0] = "0000:02:00.0"
	cloned.Entrypoint[0] = "/usr/bin/env"
	cloned.Cmd[0] = "printf"
	*cloned.HypervisorPID = 4321
	*cloned.ExitCode = 42
	cloned.AutoStandby.IgnoreSourceCIDRs[0] = "192.168.0.0/16"
	cloned.AutoStandby.IgnoreDestinationPorts[0] = 443
	cloned.HealthCheck.HTTP.Path = "/healthz"
	*cloned.SnapshotPolicy.Compression.Level = 9
	now := time.Now()
	*cloned.PendingStandbyCompression.Policy.Level = 1
	cloned.PendingStandbyCompression.NotBefore = now
	*cloned.ExpiresAt = now
	*cloned.StartedAt = now
	*cloned.StoppedAt = now

	require.Equal(t, "1", src.Env["A"])
	require.Equal(t, "x", src.Tags["m"])
	require.Equal(t, "/data", src.Volumes[0].MountPath)
	require.Equal(t, "0000:01:00.0", src.Devices[0])
	require.Equal(t, "/bin/sh", src.Entrypoint[0])
	require.Equal(t, "echo", src.Cmd[0])
	require.Equal(t, 1234, *src.HypervisorPID)
	require.Equal(t, 17, *src.ExitCode)
	require.Equal(t, "10.0.0.0/8", src.AutoStandby.IgnoreSourceCIDRs[0])
	require.Equal(t, uint16(22), src.AutoStandby.IgnoreDestinationPorts[0])
	require.Equal(t, "/", src.HealthCheck.HTTP.Path)
	require.Equal(t, 5, *src.SnapshotPolicy.Compression.Level)
	require.NotNil(t, src.PendingStandbyCompression)
	require.NotNil(t, src.PendingStandbyCompression.Policy.Level)
	require.Equal(t, 3, *src.PendingStandbyCompression.Policy.Level)
	require.Equal(t, notBefore, src.PendingStandbyCompression.NotBefore)
	require.Equal(t, expiresAt, *src.ExpiresAt)
	require.Equal(t, startedAt, *src.StartedAt)
	require.Equal(t, stoppedAt, *src.StoppedAt)
}

func TestCloneStoredMetadataWithoutPendingStandbyCompression_ClearsPendingPlan(t *testing.T) {
	t.Parallel()

	level := 4
	src := StoredMetadata{
		PendingStandbyCompression: &PendingStandbyCompression{
			Policy: snapshotstore.SnapshotCompressionConfig{
				Enabled:   true,
				Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
				Level:     &level,
			},
			NotBefore: time.Now().Add(2 * time.Minute),
		},
	}

	cloned := cloneStoredMetadataWithoutPendingStandbyCompression(src)
	assert.Nil(t, cloned.PendingStandbyCompression)
	require.NotNil(t, src.PendingStandbyCompression)
}

func TestRotateSourceVsockForRestore_CloudHypervisorDoesNotPersistCIDRewrite(t *testing.T) {
	t.Parallel()
	manager, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "fork-rotate-ch-source"
	forkID := "fork-rotate-ch-fork"
	require.NoError(t, manager.ensureDirectories(sourceID))

	snapshotConfigPath := manager.paths.InstanceSnapshotConfig(sourceID)
	require.NoError(t, os.MkdirAll(filepath.Dir(snapshotConfigPath), 0755))
	require.NoError(t, os.WriteFile(snapshotConfigPath, []byte(`{"vsock":{"cid":100,"socket":"/tmp/vsock.sock"}}`), 0644))

	meta := &metadata{StoredMetadata: StoredMetadata{
		Id:             sourceID,
		Name:           sourceID,
		CreatedAt:      time.Now(),
		HypervisorType: hypervisor.TypeCloudHypervisor,
		SocketPath:     manager.paths.InstanceSocket(sourceID, "cloud-hypervisor.sock"),
		DataDir:        manager.paths.InstanceDir(sourceID),
		VsockCID:       100,
		VsockSocket:    manager.paths.InstanceVsockSocket(sourceID),
	}}
	require.NoError(t, manager.saveMetadata(meta))

	expectedCID := generateForkSourceVsockCID(sourceID, forkID, meta.StoredMetadata.VsockCID)
	require.NotEqual(t, meta.StoredMetadata.VsockCID, expectedCID)

	require.NoError(t, manager.rotateSourceVsockForRestore(ctx, sourceID, forkID))

	updated, err := manager.loadMetadata(sourceID)
	require.NoError(t, err)
	assert.Equal(t, int64(100), updated.StoredMetadata.VsockCID)
}

func TestRotateSourceVsockForRestore_QEMUPersistsCIDRewrite(t *testing.T) {
	t.Parallel()
	manager, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "fork-rotate-qemu-source"
	forkID := "fork-rotate-qemu-fork"
	require.NoError(t, manager.ensureDirectories(sourceID))

	snapshotConfigPath := manager.paths.InstanceSnapshotConfig(sourceID)
	snapshotDir := filepath.Dir(snapshotConfigPath)
	require.NoError(t, os.MkdirAll(snapshotDir, 0755))
	require.NoError(t, os.WriteFile(snapshotConfigPath, []byte(`{}`), 0644))

	qemuConfig, err := json.Marshal(hypervisor.VMConfig{VsockCID: 100, VsockSocket: manager.paths.InstanceVsockSocket(sourceID)})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(snapshotDir, "qemu-config.json"), qemuConfig, 0644))

	meta := &metadata{StoredMetadata: StoredMetadata{
		Id:             sourceID,
		Name:           sourceID,
		CreatedAt:      time.Now(),
		HypervisorType: hypervisor.TypeQEMU,
		SocketPath:     manager.paths.InstanceSocket(sourceID, "qemu.sock"),
		DataDir:        manager.paths.InstanceDir(sourceID),
		VsockCID:       100,
		VsockSocket:    manager.paths.InstanceVsockSocket(sourceID),
	}}
	require.NoError(t, manager.saveMetadata(meta))

	expectedCID := generateForkSourceVsockCID(sourceID, forkID, meta.StoredMetadata.VsockCID)
	require.NoError(t, manager.rotateSourceVsockForRestore(ctx, sourceID, forkID))

	updated, err := manager.loadMetadata(sourceID)
	require.NoError(t, err)
	assert.Equal(t, expectedCID, updated.StoredMetadata.VsockCID)
}

func TestForkCloudHypervisorFromRunningNetwork(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available, skipping on this platform")
	}
	acquireHeavyIO(t)

	manager, tmpDir := setupTestManager(t)
	ctx := context.Background()

	imageManager, err := images.NewManager(paths.New(tmpDir), 1, nil)
	require.NoError(t, err)

	t.Log("Ensuring nginx image...")
	nginxImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{Name: integrationTestImageRef(t, "docker.io/library/nginx:alpine")})
	require.NoError(t, err)

	imageName := nginxImage.Name
	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, imageName)
		if err == nil && img.Status == images.StatusReady {
			nginxImage = img
			break
		}
		if err == nil && img.Status == images.StatusFailed {
			t.Fatalf("image build failed: %s", *img.Error)
		}
		time.Sleep(1 * time.Second)
	}
	require.Equal(t, images.StatusReady, nginxImage.Status, "Image should be ready after 60 seconds")

	systemManager := manager.systemManager
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	require.NoError(t, manager.networkManager.Initialize(ctx, nil))

	source, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fork-running-src",
		Image:          integrationTestImageRef(t, "docker.io/library/nginx:alpine"),
		Size:           2 * 1024 * 1024 * 1024,
		HotplugSize:    256 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true,
	})
	require.NoError(t, err)
	sourceID := source.Id
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), manager, sourceID) })
	source, err = waitForInstanceState(ctx, manager, source.Id, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	require.NoError(t, waitForVMReady(ctx, source.SocketPath, 5*time.Second))

	assert.NotEmpty(t, source.IP)
	assert.NotEmpty(t, source.MAC)
	assertHostCanReachNginx(t, source.IP, 80, 60*time.Second)

	// Default behavior remains strict: running source requires explicit opt-in.
	_, err = manager.ForkInstance(ctx, source.Id, ForkInstanceRequest{Name: "fork-running-no-flag"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidState)

	// Fork from running (internally: standby source -> copy fork -> restore source).
	forked, err := manager.ForkInstance(ctx, source.Id, ForkInstanceRequest{
		Name:        "fork-running-copy",
		FromRunning: true,
		TargetState: StateRunning,
	})
	require.NoError(t, err)
	require.Contains(t, []State{StateInitializing, StateRunning}, forked.State)
	forked, err = waitForInstanceState(ctx, manager, forked.Id, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	require.Equal(t, StateRunning, forked.State)
	forkedID := forked.Id
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), manager, forkedID) })

	// Source should be restored and still reachable by its private IP.
	sourceAfterFork, err := manager.GetInstance(ctx, source.Id)
	require.NoError(t, err)
	if sourceAfterFork.State != StateRunning {
		sourceAfterFork, err = waitForInstanceState(ctx, manager, source.Id, StateRunning, integrationTestTimeout(20*time.Second))
		require.NoError(t, err)
	}
	require.Equal(t, StateRunning, sourceAfterFork.State)
	require.NotEmpty(t, sourceAfterFork.IP)
	assertHostCanReachNginx(t, sourceAfterFork.IP, 80, 60*time.Second)

	// Fork should already be running with target_state=Running.
	require.NoError(t, waitForVMReady(ctx, forked.SocketPath, 5*time.Second))

	assert.NotEmpty(t, forked.IP)
	assert.NotEmpty(t, forked.MAC)
	assert.NotEqual(t, sourceAfterFork.IP, forked.IP)
	assert.NotEqual(t, sourceAfterFork.MAC, forked.MAC)
	assertGuestHasOnlyExpectedIPv4(t, forked, forked.IP, 30*time.Second)
	assertHostCanReachNginx(t, forked.IP, 80, 60*time.Second)

	// Fork must start with a fresh phase ledger and not inherit the source's
	// accumulated running time. The source has completed at least one running
	// stint by now (running -> internal-standby -> running); the fork is still
	// in its first running stint and so has no completed running ms logged.
	assert.Equal(t, phasetracking.PhaseRunning, forked.Phases.Current)
	assert.Zero(t, forked.Phases.Cumulative[phasetracking.PhaseRunning], "fork should not inherit source's running ledger")
	assert.Greater(t, sourceAfterFork.Phases.Cumulative[phasetracking.PhaseRunning], int64(0), "source's pre-fork running stint should be cumulated")
}

func TestCloudHypervisorWarmForkChain(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available, skipping on this platform")
	}

	manager, tmpDir := setupTestManager(t)
	runWarmForkChain(t, manager, tmpDir, warmForkChainConfig{
		hypervisor: hypervisor.TypeCloudHypervisor,
		namePrefix: "ch",
	})
}

type warmForkChainConfig struct {
	hypervisor hypervisor.Type
	namePrefix string
}

func runWarmForkChain(t *testing.T, mgr *manager, tmpDir string, cfg warmForkChainConfig) {
	t.Helper()
	acquireHeavyIO(t)

	ctx := context.Background()
	readyTimeout := 90 * time.Second
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	imageName := integrationTestImageRef(t, "docker.io/library/nginx:alpine")
	snapshottest.EnsureImageReady(t, ctx, p, imageManager, imageName)

	require.NoError(t, mgr.systemManager.EnsureSystemFiles(ctx))

	source, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           cfg.namePrefix + "-warm-chain-src",
		Image:          imageName,
		Size:           lifecycleTestMemorySize,
		OverlaySize:    1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     cfg.hypervisor,
		Entrypoint:     []string{"/bin/sleep"},
		Cmd:            []string{"86400"},
	})
	require.NoError(t, err)
	require.Equal(t, cfg.hypervisor, source.HypervisorType)
	sourceID := source.Id
	sourceDeleted := false
	t.Cleanup(func() {
		if !sourceDeleted {
			_ = deleteTestInstanceNow(context.Background(), mgr, sourceID)
		}
	})

	source, err = waitForInstanceState(ctx, mgr, sourceID, StateRunning, readyTimeout)
	require.NoError(t, err)
	require.NoError(t, waitForExecAgent(ctx, mgr, sourceID, readyTimeout))

	snapshot, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: cfg.namePrefix + "-warm-chain-snap",
	})
	require.NoError(t, err)
	require.Equal(t, SnapshotKindStandby, snapshot.Kind)
	snapshotDeleted := false
	t.Cleanup(func() {
		if !snapshotDeleted {
			_ = mgr.DeleteSnapshot(context.Background(), snapshot.Id)
		}
	})

	require.NoError(t, deleteTestInstanceNow(ctx, mgr, sourceID))
	sourceDeleted = true

	warm, err := mgr.ForkSnapshot(ctx, snapshot.Id, ForkSnapshotRequest{
		Name:        cfg.namePrefix + "-warm-chain-warm",
		TargetState: StateRunning,
	})
	require.NoError(t, err)
	require.Equal(t, cfg.hypervisor, warm.HypervisorType)
	warmID := warm.Id
	warmDeleted := false
	t.Cleanup(func() {
		if !warmDeleted {
			_ = deleteTestInstanceNow(context.Background(), mgr, warmID)
		}
	})
	warm, err = waitForInstanceState(ctx, mgr, warmID, StateRunning, readyTimeout)
	require.NoError(t, err)
	require.NoError(t, waitForExecAgent(ctx, mgr, warmID, readyTimeout))

	child, err := mgr.ForkInstance(ctx, warmID, ForkInstanceRequest{
		Name:        cfg.namePrefix + "-warm-chain-child",
		FromRunning: true,
		TargetState: StateStopped,
	})
	require.NoError(t, err)
	require.Equal(t, StateStopped, child.State)
	require.Equal(t, cfg.hypervisor, child.HypervisorType)
	childID := child.Id
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), mgr, childID) })

	warm, err = mgr.GetInstance(ctx, warmID)
	require.NoError(t, err)
	if warm.State != StateRunning {
		warm, err = waitForInstanceState(ctx, mgr, warmID, StateRunning, readyTimeout)
		require.NoError(t, err)
	}
	require.Equal(t, StateRunning, warm.State)
	require.NoError(t, waitForExecAgent(ctx, mgr, warmID, readyTimeout))

	require.NoError(t, deleteTestInstanceNow(ctx, mgr, warmID))
	warmDeleted = true
	require.NoError(t, mgr.DeleteSnapshot(ctx, snapshot.Id))
	snapshotDeleted = true
}

func assertHostCanReachNginx(t *testing.T, ip string, port int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://%s:%d/", ip, port)
	client := &http.Client{Timeout: 3 * time.Second}

	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK && strings.Contains(string(body), "Welcome to nginx!") {
				return
			}
			if readErr != nil {
				lastErr = fmt.Errorf("read body: %w", readErr)
			} else {
				lastErr = fmt.Errorf("status=%d body=%q", resp.StatusCode, string(body))
			}
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}

	require.NoError(t, lastErr, "host should reach %s within %s", url, timeout)
}

func assertGuestHasOnlyExpectedIPv4(t *testing.T, inst *Instance, expectedIP string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		output, exitCode, err := execInInstance(context.Background(), inst, "sh", "-c", "ip -4 -o addr show dev eth0 scope global | awk '{print $4}'")
		if err == nil && exitCode == 0 {
			var cidrs []string
			for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
				if trimmed := strings.TrimSpace(line); trimmed != "" {
					cidrs = append(cidrs, trimmed)
				}
			}

			if len(cidrs) == 1 && strings.HasPrefix(cidrs[0], expectedIP+"/") {
				return
			}

			lastErr = fmt.Errorf("expected only %s on eth0, got %v", expectedIP, cidrs)
		} else if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("ip addr command exit code %d, output=%q", exitCode, output)
		}

		time.Sleep(500 * time.Millisecond)
	}

	require.NoError(t, lastErr, "guest should expose only the fork IP on eth0 within %s", timeout)
}

func execInInstance(ctx context.Context, inst *Instance, command ...string) (string, int, error) {
	dialer, err := hypervisor.NewVsockDialer(inst.HypervisorType, inst.VsockSocket, inst.VsockCID)
	if err != nil {
		return "", -1, err
	}

	var stdout, stderr bytes.Buffer
	exit, err := guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command:      command,
		Stdout:       &stdout,
		Stderr:       &stderr,
		WaitForAgent: 2 * time.Second,
	})
	if err != nil {
		return "", -1, err
	}

	output := stdout.String()
	if stderr.Len() > 0 {
		output += "\nSTDERR: " + stderr.String()
	}
	return output, exit.Code, nil
}
