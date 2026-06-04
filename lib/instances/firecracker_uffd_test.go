package instances

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/uffdpager"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFirecrackerSnapshotRestoreOptionsOneShotUFFDGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		backend     string
		useNext     bool
		wantBackend hypervisor.SnapshotMemoryBackend
		wantErr     bool
	}{
		{
			name:        "config file ignores armed one-shot flag",
			backend:     uffdpager.BackendFile,
			useNext:     true,
			wantBackend: hypervisor.SnapshotMemoryBackendFile,
		},
		{
			name:        "config uffd with unarmed flag uses file backend",
			backend:     uffdpager.BackendUFFD,
			useNext:     false,
			wantBackend: hypervisor.SnapshotMemoryBackendFile,
		},
		{
			name:    "config uffd with armed flag requires pager",
			backend: uffdpager.BackendUFFD,
			useNext: true,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mgr := &manager{firecrackerSnapshotMemoryBackend: tc.backend}
			stored := &StoredMetadata{
				Id:                              "fc-restore-options",
				HypervisorType:                  hypervisor.TypeFirecracker,
				FirecrackerSnapshotCacheKey:     "snapshot-cache-key",
				FirecrackerUseUFFDOnNextRestore: tc.useNext,
				FirecrackerUFFDSessionID:        "stale-session",
				FirecrackerUFFDPagerVersion:     "stale-version",
			}

			opts, err := mgr.firecrackerSnapshotRestoreOptions(stored, t.TempDir())
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "pager is not configured")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantBackend, opts.SnapshotMemoryBackend)
			assert.Empty(t, opts.SnapshotMemoryCacheKey)
			assert.Empty(t, opts.SnapshotMemorySessionID)
			assert.Empty(t, stored.FirecrackerUFFDSessionID)
			assert.Empty(t, stored.FirecrackerUFFDPagerVersion)
		})
	}
}

func TestForkInstanceFromStandbyArmsOneShotUFFDForFirecrackerRestore(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	installOneShotFirecrackerStarter(t, mgr)
	ctx := context.Background()

	sourceID := "one-shot-uffd-instance-source"
	createStandbySnapshotSourceFixture(t, mgr, sourceID, sourceID, hypervisor.TypeFirecracker)
	sourceMemoryPath := firecrackerSnapshotMemoryPathInGuestDir(mgr.paths.InstanceDir(sourceID))
	require.NoError(t, os.WriteFile(sourceMemoryPath, []byte("source memory"), 0644))
	sourceMeta, err := mgr.loadMetadata(sourceID)
	require.NoError(t, err)
	sourceMeta.StoredMetadata.FirecrackerSnapshotCacheKey = "shared-template-cache"
	require.NoError(t, mgr.saveMetadata(sourceMeta))
	mgr.firecrackerSnapshotMemoryBackend = uffdpager.BackendUFFD

	forked, err := mgr.forkInstanceFromStoppedOrStandby(ctx, sourceID, ForkInstanceRequest{
		Name:        "one-shot-uffd-instance-fork",
		TargetState: StateStandby,
	}, true)
	require.NoError(t, err)

	meta, err := mgr.loadMetadata(forked.Id)
	require.NoError(t, err)
	assert.True(t, meta.StoredMetadata.FirecrackerUseUFFDOnNextRestore)
	assert.Equal(t, "shared-template-cache", meta.StoredMetadata.FirecrackerSnapshotCacheKey)
	assert.Equal(t, sourceMemoryPath, meta.StoredMetadata.FirecrackerDeferredSnapshotMemoryPath)
	assert.Empty(t, meta.StoredMetadata.FirecrackerUFFDSessionID)
	assert.Empty(t, meta.StoredMetadata.FirecrackerUFFDPagerVersion)
	assert.NoFileExists(t, firecrackerSnapshotMemoryPathInGuestDir(mgr.paths.InstanceDir(forked.Id)))
	assert.FileExists(t, filepath.Join(mgr.paths.InstanceSnapshotLatest(forked.Id), "state"))
}

func TestForkInstanceFromStandbyDoesNotArmOneShotUFFDForStoppedTarget(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	installOneShotFirecrackerStarter(t, mgr)
	ctx := context.Background()

	sourceID := "one-shot-uffd-instance-stopped-source"
	createStandbySnapshotSourceFixture(t, mgr, sourceID, sourceID, hypervisor.TypeFirecracker)
	sourceMemoryPath := firecrackerSnapshotMemoryPathInGuestDir(mgr.paths.InstanceDir(sourceID))
	require.NoError(t, os.WriteFile(sourceMemoryPath, []byte("source memory"), 0644))
	sourceMeta, err := mgr.loadMetadata(sourceID)
	require.NoError(t, err)
	sourceMeta.StoredMetadata.FirecrackerSnapshotCacheKey = "shared-template-cache"
	require.NoError(t, mgr.saveMetadata(sourceMeta))
	mgr.firecrackerSnapshotMemoryBackend = uffdpager.BackendUFFD

	forked, err := mgr.forkInstanceFromStoppedOrStandby(ctx, sourceID, ForkInstanceRequest{
		Name:        "one-shot-uffd-instance-stopped-fork",
		TargetState: StateStopped,
	}, true)
	require.NoError(t, err)

	meta, err := mgr.loadMetadata(forked.Id)
	require.NoError(t, err)
	assert.False(t, meta.StoredMetadata.FirecrackerUseUFFDOnNextRestore)
	assert.Equal(t, "shared-template-cache", meta.StoredMetadata.FirecrackerSnapshotCacheKey)
	assert.Empty(t, meta.StoredMetadata.FirecrackerDeferredSnapshotMemoryPath)
	assert.FileExists(t, firecrackerSnapshotMemoryPathInGuestDir(mgr.paths.InstanceDir(forked.Id)))
}

func TestForkSnapshotFromStandbyArmsOneShotUFFDForFirecrackerRestore(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	installOneShotFirecrackerStarter(t, mgr)
	ctx := context.Background()

	sourceID := "one-shot-uffd-snapshot-source"
	createStandbySnapshotSourceFixture(t, mgr, sourceID, sourceID, hypervisor.TypeFirecracker)
	require.NoError(t, os.WriteFile(firecrackerSnapshotMemoryPathInGuestDir(mgr.paths.InstanceDir(sourceID)), []byte("source memory"), 0644))
	sourceMeta, err := mgr.loadMetadata(sourceID)
	require.NoError(t, err)
	sourceMeta.StoredMetadata.FirecrackerSnapshotCacheKey = "shared-snapshot-cache"
	require.NoError(t, mgr.saveMetadata(sourceMeta))
	mgr.firecrackerSnapshotMemoryBackend = uffdpager.BackendUFFD

	snap, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: "one-shot-uffd-snapshot",
	})
	require.NoError(t, err)

	forked, err := mgr.ForkSnapshot(ctx, snap.Id, ForkSnapshotRequest{
		Name:        "one-shot-uffd-snapshot-fork",
		TargetState: StateStandby,
	})
	require.NoError(t, err)

	meta, err := mgr.loadMetadata(forked.Id)
	require.NoError(t, err)
	assert.True(t, meta.StoredMetadata.FirecrackerUseUFFDOnNextRestore)
	assert.Equal(t, "shared-snapshot-cache", meta.StoredMetadata.FirecrackerSnapshotCacheKey)
	assert.Equal(t, firecrackerSnapshotMemoryPathInGuestDir(mgr.paths.SnapshotGuestDir(snap.Id)), meta.StoredMetadata.FirecrackerDeferredSnapshotMemoryPath)
	assert.Empty(t, meta.StoredMetadata.FirecrackerUFFDSessionID)
	assert.Empty(t, meta.StoredMetadata.FirecrackerUFFDPagerVersion)
	assert.NoFileExists(t, firecrackerSnapshotMemoryPathInGuestDir(mgr.paths.InstanceDir(forked.Id)))
	assert.FileExists(t, filepath.Join(mgr.paths.InstanceSnapshotLatest(forked.Id), "state"))
}

func TestForkSnapshotFromStoppedDoesNotArmOneShotUFFD(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	installOneShotFirecrackerStarter(t, mgr)
	ctx := context.Background()

	sourceID := "one-shot-uffd-stopped-snapshot-source"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, sourceID, hypervisor.TypeFirecracker)
	sourceMeta, err := mgr.loadMetadata(sourceID)
	require.NoError(t, err)
	sourceMeta.StoredMetadata.FirecrackerSnapshotCacheKey = "stopped-snapshot-cache"
	require.NoError(t, mgr.saveMetadata(sourceMeta))

	snap, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStopped,
		Name: "one-shot-uffd-stopped-snapshot",
	})
	require.NoError(t, err)

	forked, err := mgr.ForkSnapshot(ctx, snap.Id, ForkSnapshotRequest{
		Name:        "one-shot-uffd-stopped-snapshot-fork",
		TargetState: StateStopped,
	})
	require.NoError(t, err)

	meta, err := mgr.loadMetadata(forked.Id)
	require.NoError(t, err)
	assert.False(t, meta.StoredMetadata.FirecrackerUseUFFDOnNextRestore)
	assert.Empty(t, meta.StoredMetadata.FirecrackerSnapshotCacheKey)
}

func TestRestoreInstanceClearsOneShotUFFDFlagAfterSuccessfulRestore(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	installOneShotFirecrackerStarter(t, mgr)
	mgr.firecrackerSnapshotMemoryBackend = uffdpager.BackendFile
	ctx := context.Background()

	id := "one-shot-uffd-restore-clear"
	createStandbySnapshotSourceFixture(t, mgr, id, id, hypervisor.TypeFirecracker)
	meta, err := mgr.loadMetadata(id)
	require.NoError(t, err)
	meta.StoredMetadata.FirecrackerSnapshotCacheKey = "shared-template-cache"
	meta.StoredMetadata.FirecrackerUseUFFDOnNextRestore = true
	require.NoError(t, mgr.saveMetadata(meta))

	inst, err := mgr.restoreInstance(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, inst)

	updated, err := mgr.loadMetadata(id)
	require.NoError(t, err)
	assert.False(t, updated.StoredMetadata.FirecrackerUseUFFDOnNextRestore)
}

func installOneShotFirecrackerStarter(t *testing.T, mgr *manager) {
	t.Helper()
	previous, hadPrevious := mgr.vmStarters[hypervisor.TypeFirecracker]
	mgr.vmStarters[hypervisor.TypeFirecracker] = oneShotFirecrackerTestStarter{}
	t.Cleanup(func() {
		if hadPrevious {
			mgr.vmStarters[hypervisor.TypeFirecracker] = previous
			return
		}
		delete(mgr.vmStarters, hypervisor.TypeFirecracker)
	})
}

type oneShotFirecrackerTestStarter struct{}

func (oneShotFirecrackerTestStarter) SocketName() string {
	return "firecracker.sock"
}

func (oneShotFirecrackerTestStarter) GetBinaryPath(*paths.Paths, string) (string, error) {
	return "/bin/true", nil
}

func (oneShotFirecrackerTestStarter) GetVersion(*paths.Paths) (string, error) {
	return "test", nil
}

func (oneShotFirecrackerTestStarter) StartVM(context.Context, *paths.Paths, string, string, hypervisor.VMConfig) (int, hypervisor.Hypervisor, error) {
	return 1234, oneShotFirecrackerTestHypervisor{}, nil
}

func (oneShotFirecrackerTestStarter) RestoreVM(context.Context, *paths.Paths, string, string, string, hypervisor.RestoreOptions) (int, hypervisor.Hypervisor, error) {
	return 1234, oneShotFirecrackerTestHypervisor{}, nil
}

func (oneShotFirecrackerTestStarter) PrepareFork(context.Context, hypervisor.ForkPrepareRequest) (hypervisor.ForkPrepareResult, error) {
	return hypervisor.ForkPrepareResult{}, nil
}

type oneShotFirecrackerTestHypervisor struct{}

func (oneShotFirecrackerTestHypervisor) DeleteVM(context.Context) error {
	return nil
}

func (oneShotFirecrackerTestHypervisor) Shutdown(context.Context) error {
	return nil
}

func (oneShotFirecrackerTestHypervisor) GetVMInfo(context.Context) (*hypervisor.VMInfo, error) {
	return &hypervisor.VMInfo{State: hypervisor.StateRunning}, nil
}

func (oneShotFirecrackerTestHypervisor) Pause(context.Context) error {
	return nil
}

func (oneShotFirecrackerTestHypervisor) Resume(context.Context) error {
	return nil
}

func (oneShotFirecrackerTestHypervisor) Snapshot(context.Context, string) error {
	return nil
}

func (oneShotFirecrackerTestHypervisor) ResizeMemory(context.Context, int64) error {
	return nil
}

func (oneShotFirecrackerTestHypervisor) ResizeMemoryAndWait(context.Context, int64, time.Duration) error {
	return nil
}

func (oneShotFirecrackerTestHypervisor) SetTargetGuestMemoryBytes(context.Context, int64) error {
	return nil
}

func (oneShotFirecrackerTestHypervisor) GetTargetGuestMemoryBytes(context.Context) (int64, error) {
	return 0, nil
}

func (oneShotFirecrackerTestHypervisor) Capabilities() hypervisor.Capabilities {
	return hypervisor.Capabilities{SupportsSnapshot: true, SupportsPause: true}
}
