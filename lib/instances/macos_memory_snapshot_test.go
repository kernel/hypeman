package instances

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
	"github.com/kernel/hypeman/lib/paths"
	restartpolicy "github.com/kernel/hypeman/lib/restart-policy"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/stretchr/testify/require"
)

const macOSMemoryTestHypervisor hypervisor.Type = "macos-memory-snapshot-test"

func macOSMemorySnapshotFixture(t *testing.T) (*manager, *metadata) {
	t.Helper()
	hypervisor.RegisterCapabilities(macOSMemoryTestHypervisor, hypervisor.Capabilities{SupportsMacOSBoot: true, SupportsSnapshot: true})
	m := &manager{paths: paths.New(t.TempDir()), macOSOnly: true, now: time.Now, lifecycleEvents: newLifecycleSubscribers(), vmStarters: map[hypervisor.Type]hypervisor.VMStarter{macOSMemoryTestHypervisor: concurrentForkPrepareTestStarter{}}}
	m.deleteSnapshotFn = m.deleteSnapshot
	const id = "mac-memory-source"
	createStoppedSnapshotSourceFixture(t, m, id, id, macOSMemoryTestHypervisor)
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.MacOS = macOSGuestMetadata(2).MacOS
	meta.Size, meta.Vcpus = int64(meta.MacOS.Memory), int(meta.MacOS.CPUs)
	meta.MAC, meta.NetworkEnabled = meta.MacOS.MAC, true
	meta.SkipGuestAgent = true
	meta.StopGraceful = true
	require.NoError(t, m.saveMetadata(meta))
	require.NoError(t, os.WriteFile(filepath.Join(meta.DataDir, macOSAuxStorageFile), []byte("aux-at-checkpoint"), 0600))
	return m, meta
}

func writeMacOSMemorySnapshotPayload(t *testing.T, m *manager, guestDir string, stored *StoredMetadata) shimconfig.SnapshotManifest {
	t.Helper()
	snapshotDir := filepath.Join(guestDir, "snapshots", "snapshot-latest")
	require.NoError(t, os.MkdirAll(snapshotDir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(snapshotDir, shimconfig.SnapshotMachineStateFile), []byte("saved-memory"), 0600))
	manifest := shimconfig.SnapshotManifest{Hypervisor: "vz", MachineStateFile: shimconfig.SnapshotMachineStateFile, ShimConfig: shimconfig.ShimConfig{
		VCPUs: stored.Vcpus, MemoryBytes: stored.Size,
		MacHardwareModelData: base64.StdEncoding.EncodeToString(stored.MacOS.HardwareModel), MacMachineIdentifierData: base64.StdEncoding.EncodeToString(stored.MacOS.MachineIdentifier),
		MacAuxStoragePath: filepath.Join(m.paths.InstanceDir(stored.Id), macOSAuxStorageFile),
		Disks:             []shimconfig.DiskConfig{{Path: m.paths.InstanceOverlay(stored.Id)}},
	}}
	if stored.NetworkEnabled {
		manifest.ShimConfig.Networks = []shimconfig.NetworkConfig{{MAC: stored.MAC}}
	}
	writeMacOSSnapshotManifest(t, snapshotDir, manifest)
	return manifest
}

func writeMacOSSnapshotManifest(t *testing.T, snapshotDir string, manifest shimconfig.SnapshotManifest) {
	t.Helper()
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(snapshotDir, shimconfig.SnapshotManifestFile), data, 0600))
}

func TestMacOSMemorySnapshotCaptureRestoreAndIsolation(t *testing.T) {
	m, meta := macOSMemorySnapshotFixture(t)
	writeMacOSMemorySnapshotPayload(t, m, meta.DataDir, &meta.StoredMetadata)
	meta.StopGraceful = false
	bootTime := time.Now().Add(-2 * time.Hour)
	checkpointTime := bootTime.Add(time.Hour)
	meta.StartedAt = &bootTime
	meta.Phases.Record(phasetracking.PhaseRunning, bootTime)
	meta.Phases.Record(phasetracking.PhaseStandby, checkpointTime)
	require.NoError(t, m.saveMetadata(meta))
	// Linux compression defaults must not start a codec job for VZ machine state.
	m.snapshotDefaults.Compression = &snapshotstore.SnapshotCompressionConfig{Enabled: true, Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd}
	ctx := context.Background()
	snapshot, err := m.CreateSnapshot(ctx, meta.Id, CreateSnapshotRequest{Kind: SnapshotKindStandby})
	require.NoError(t, err)
	require.Equal(t, SnapshotKindStandby, snapshot.Kind)
	require.Equal(t, snapshotstore.SnapshotCompressionStateNone, snapshot.CompressionState)
	source, err := m.GetInstance(ctx, meta.Id)
	require.NoError(t, err)
	require.Equal(t, StateStandby, source.State)
	_, err = m.ForkSnapshot(ctx, snapshot.Id, ForkSnapshotRequest{Name: "unsafe-warm-fork"})
	require.ErrorIs(t, err, ErrNotSupported)
	current, err := m.loadMetadata(meta.Id)
	require.NoError(t, err)
	current.Phases.Record(phasetracking.PhaseRunning, checkpointTime.Add(10*time.Minute))
	current.Phases.Record(phasetracking.PhaseStandby, checkpointTime.Add(20*time.Minute))
	expiresAt := time.Now().Add(3 * time.Hour)
	current.ExpiresAt = &expiresAt
	require.NoError(t, m.saveMetadata(current))

	require.NoError(t, os.WriteFile(m.paths.InstanceOverlay(meta.Id), []byte("after-checkpoint"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(meta.DataDir, macOSAuxStorageFile), []byte("aux-after"), 0600))
	restored, err := m.RestoreSnapshot(ctx, meta.Id, snapshot.Id, RestoreSnapshotRequest{TargetState: StateStandby})
	require.NoError(t, err)
	require.Equal(t, StateStandby, restored.State)
	require.Equal(t, meta.MacOS.MachineIdentifier, restored.MacOS.MachineIdentifier)
	require.Equal(t, meta.MAC, restored.MAC)
	require.NotNil(t, restored.StartedAt)
	require.True(t, bootTime.Equal(*restored.StartedAt), "a memory restore resumes the checkpoint's boot")
	require.Equal(t, current.Phases.Cumulative[phasetracking.PhaseRunning], restored.Phases.Cumulative[phasetracking.PhaseRunning], "same-instance restore must not rewind host phase accounting")
	require.NotNil(t, restored.ExpiresAt)
	require.True(t, expiresAt.Equal(*restored.ExpiresAt))
	for name, want := range map[string]string{"overlay.raw": "overlay", macOSAuxStorageFile: "aux-at-checkpoint", "snapshots/snapshot-latest/" + shimconfig.SnapshotMachineStateFile: "saved-memory"} {
		data, err := os.ReadFile(filepath.Join(meta.DataDir, name))
		require.NoError(t, err)
		require.Equal(t, want, string(data))
	}
	require.NoError(t, os.WriteFile(m.paths.InstanceOverlay(meta.Id), []byte("restored-write"), 0600))
	data, err := os.ReadFile(filepath.Join(m.paths.SnapshotGuestDir(snapshot.Id), "overlay.raw"))
	require.NoError(t, err)
	require.Equal(t, "overlay", string(data), "restoring must not alias the persistent snapshot")

	restored, err = m.RestoreSnapshot(ctx, meta.Id, snapshot.Id, RestoreSnapshotRequest{TargetState: StateStopped})
	require.NoError(t, err)
	require.Equal(t, StateStopped, restored.State)
	require.False(t, restored.StopGraceful, "discarding memory does not establish a clean shutdown receipt")
	require.Equal(t, restartpolicy.BlockedReasonManualStop, restored.RestartStatus.BlockedReason, "an explicit stopped target must not be automatically restarted")
	_, err = m.CreateSnapshot(ctx, meta.Id, CreateSnapshotRequest{Kind: SnapshotKindStopped})
	require.ErrorIs(t, err, ErrInvalidState)
}

func TestMacOSMemorySnapshotRejectsInvalidPayloadBeforeStorageSwap(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*shimconfig.SnapshotManifest)
	}{
		{"cpu", func(m *shimconfig.SnapshotManifest) { m.ShimConfig.VCPUs++ }},
		{"ram", func(m *shimconfig.SnapshotManifest) { m.ShimConfig.MemoryBytes++ }},
		{"identity", func(m *shimconfig.SnapshotManifest) { m.ShimConfig.MacMachineIdentifierData = "different" }},
		{"hardware", func(m *shimconfig.SnapshotManifest) { m.ShimConfig.MacHardwareModelData = "different" }},
		{"disk", func(m *shimconfig.SnapshotManifest) { m.ShimConfig.Disks[0].Path = "/wrong-disk" }},
		{"aux", func(m *shimconfig.SnapshotManifest) { m.ShimConfig.MacAuxStoragePath = "/wrong-aux" }},
		{"network", func(m *shimconfig.SnapshotManifest) { m.ShimConfig.Networks[0].MAC = "02:00:00:00:00:02" }},
		{"state-path", func(m *shimconfig.SnapshotManifest) { m.MachineStateFile = "../../other-state" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, meta := macOSMemorySnapshotFixture(t)
			writeMacOSMemorySnapshotPayload(t, m, meta.DataDir, &meta.StoredMetadata)
			snapshot, err := m.CreateSnapshot(context.Background(), meta.Id, CreateSnapshotRequest{Kind: SnapshotKindStandby})
			require.NoError(t, err)
			snapshotDir := filepath.Join(m.paths.SnapshotGuestDir(snapshot.Id), "snapshots", "snapshot-latest")
			rec, err := m.loadSnapshotRecord(snapshot.Id)
			require.NoError(t, err)
			manifest := writeMacOSMemorySnapshotPayload(t, m, m.paths.SnapshotGuestDir(snapshot.Id), &rec.StoredMetadata)
			test.mutate(&manifest)
			writeMacOSSnapshotManifest(t, snapshotDir, manifest)
			require.NoError(t, os.WriteFile(m.paths.InstanceOverlay(meta.Id), []byte("current-disk"), 0600))
			_, err = m.RestoreSnapshot(context.Background(), meta.Id, snapshot.Id, RestoreSnapshotRequest{})
			require.ErrorIs(t, err, ErrInvalidRequest)
			data, err := os.ReadFile(m.paths.InstanceOverlay(meta.Id))
			require.NoError(t, err)
			require.Equal(t, "current-disk", string(data))
		})
	}
}

type macOSSnapshotProbeTestHypervisor struct {
	lifecycleNoopHypervisor
	supportErr  error
	probeCalls  int
	pauseCalls  int
	resumeCalls int
}

func (h *macOSSnapshotProbeTestHypervisor) ValidateSnapshotSupport(context.Context) error {
	h.probeCalls++
	return h.supportErr
}

func (h *macOSSnapshotProbeTestHypervisor) Pause(context.Context) error {
	h.pauseCalls++
	return nil
}

func (h *macOSSnapshotProbeTestHypervisor) Capabilities() hypervisor.Capabilities {
	return hypervisor.Capabilities{SupportsSnapshot: true}
}

func (h *macOSSnapshotProbeTestHypervisor) Resume(ctx context.Context) error {
	h.resumeCalls++
	return ctx.Err()
}

func TestMacOSSnapshotConfigurationProbeAndPauseRecovery(t *testing.T) {
	stored := macOSGuestMetadata(2)
	hv := &macOSSnapshotProbeTestHypervisor{supportErr: hypervisor.ErrNotSupported}
	require.ErrorIs(t, validateMacOSSaveRestoreSupport(context.Background(), stored, hv), ErrNotSupported)
	require.Equal(t, 1, hv.probeCalls)
	require.NoError(t, validateMacOSSaveRestoreSupport(context.Background(), &StoredMetadata{}, hv))
	require.Equal(t, 1, hv.probeCalls, "Linux must not call the macOS configuration probe")
	require.ErrorIs(t, validateMacOSSaveRestoreSupport(context.Background(), stored, lifecycleNoopHypervisor{}), ErrNotSupported)
	hv.supportErr = nil
	require.NoError(t, validateMacOSSaveRestoreSupport(context.Background(), stored, hv))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, resumeSnapshotAfterError(ctx, stored, hv))
	require.Equal(t, 1, hv.resumeCalls)
}

func TestMacOSLiveConfigurationRejectedBeforePauseAndSchedulePersistence(t *testing.T) {
	m, meta := macOSMemorySnapshotFixture(t)
	const hvType hypervisor.Type = "macos-live-snapshot-probe-test"
	hv := &macOSSnapshotProbeTestHypervisor{lifecycleNoopHypervisor: lifecycleNoopHypervisor{state: hypervisor.StateRunning}, supportErr: hypervisor.ErrNotSupported}
	hypervisor.RegisterCapabilities(hvType, hypervisor.Capabilities{SupportsMacOSBoot: true, SupportsSnapshot: true})
	hypervisor.RegisterClientFactory(hvType, func(string) (hypervisor.Hypervisor, error) { return hv, nil })
	meta.HypervisorType = hvType
	require.NoError(t, m.saveMetadata(meta))
	require.NoError(t, os.WriteFile(meta.SocketPath, nil, 0600))
	ctx := context.Background()
	_, err := m.SetSnapshotSchedule(ctx, meta.Id, SetSnapshotScheduleRequest{Interval: time.Hour, NamePrefix: "unsupported", Retention: SnapshotScheduleRetention{MaxCount: 1}})
	require.ErrorIs(t, err, ErrNotSupported)
	_, err = m.GetSnapshotSchedule(ctx, meta.Id)
	require.ErrorIs(t, err, ErrSnapshotScheduleNotFound)
	_, err = m.CreateSnapshot(ctx, meta.Id, CreateSnapshotRequest{Kind: SnapshotKindStandby})
	require.ErrorIs(t, err, ErrNotSupported)
	require.Equal(t, 2, hv.probeCalls, "configuration validation must pass through the tracing wrapper")
	require.Equal(t, 0, hv.pauseCalls, "unsupported configurations are rejected before pausing")
	inst, err := m.GetInstance(ctx, meta.Id)
	require.NoError(t, err)
	require.Equal(t, StateRunning, inst.State)
}

type macOSCancelledSnapshotTestHypervisor struct {
	macOSSnapshotProbeTestHypervisor
	cancel       context.CancelFunc
	failStage    string
	writePayload func()
}

func (h *macOSCancelledSnapshotTestHypervisor) Snapshot(ctx context.Context, _ string) error {
	if h.failStage == "snapshot" {
		h.cancel()
		return ctx.Err()
	}
	h.writePayload()
	return nil
}

func (h *macOSCancelledSnapshotTestHypervisor) Capabilities() hypervisor.Capabilities {
	return hypervisor.Capabilities{SupportsSnapshot: true, SupportsGracefulVMMShutdown: true}
}

func (h *macOSCancelledSnapshotTestHypervisor) Shutdown(ctx context.Context) error {
	h.cancel()
	return ctx.Err()
}

func TestMacOSSnapshotCancellationRecoversSource(t *testing.T) {
	for _, stage := range []string{"snapshot", "shutdown"} {
		t.Run(stage, func(t *testing.T) {
			m, meta := macOSMemorySnapshotFixture(t)
			hvType := hypervisor.Type("macos-cancelled-" + stage + "-test")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hv := &macOSCancelledSnapshotTestHypervisor{macOSSnapshotProbeTestHypervisor: macOSSnapshotProbeTestHypervisor{lifecycleNoopHypervisor: lifecycleNoopHypervisor{state: hypervisor.StateRunning}}, cancel: cancel, failStage: stage}
			hypervisor.RegisterCapabilities(hvType, hypervisor.Capabilities{SupportsMacOSBoot: true, SupportsSnapshot: true})
			hypervisor.RegisterClientFactory(hvType, func(string) (hypervisor.Hypervisor, error) { return hv, nil })
			meta.HypervisorType = hvType
			hv.writePayload = func() { writeMacOSMemorySnapshotPayload(t, m, meta.DataDir, &meta.StoredMetadata) }
			m.vmStarters[hvType] = macOSSnapshotRestoreContextStarter{}
			require.NoError(t, m.saveMetadata(meta))
			require.NoError(t, os.WriteFile(meta.SocketPath, nil, 0600))
			_, err := m.CreateSnapshot(ctx, meta.Id, CreateSnapshotRequest{Kind: SnapshotKindStandby})
			if stage == "snapshot" {
				require.ErrorIs(t, err, context.Canceled)
				require.Equal(t, 1, hv.resumeCalls)
			} else {
				require.NoError(t, err, "confirmed shutdown must recover the source despite cancellation")
			}
			require.Equal(t, 1, hv.pauseCalls)
			inst, err := m.GetInstance(context.Background(), meta.Id)
			require.NoError(t, err)
			require.Equal(t, StateRunning, inst.State)
		})
	}
}

type macOSSnapshotRestoreContextStarter struct {
	concurrentForkPrepareTestStarter
}

func (s macOSSnapshotRestoreContextStarter) RestoreVM(ctx context.Context, p *paths.Paths, version, socket, snapshot string, options hypervisor.RestoreOptions) (int, hypervisor.Hypervisor, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	return s.concurrentForkPrepareTestStarter.RestoreVM(ctx, p, version, socket, snapshot, options)
}

func TestMacOSSnapshotRestoresSourceAfterRequestCancellation(t *testing.T) {
	m, meta := macOSMemorySnapshotFixture(t)
	writeMacOSMemorySnapshotPayload(t, m, meta.DataDir, &meta.StoredMetadata)
	m.vmStarters[macOSMemoryTestHypervisor] = macOSSnapshotRestoreContextStarter{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, m.restoreSnapshotSource(ctx, &meta.StoredMetadata))
	inst, err := m.GetInstance(context.Background(), meta.Id)
	require.NoError(t, err)
	require.Equal(t, StateRunning, inst.State)
}

func TestMacOSMemorySnapshotRequiresPayloadAndRejectsCompression(t *testing.T) {
	m, meta := macOSMemorySnapshotFixture(t)
	_, err := m.CreateSnapshot(context.Background(), meta.Id, CreateSnapshotRequest{Kind: SnapshotKindStandby, Compression: &snapshotstore.SnapshotCompressionConfig{Enabled: true}})
	require.ErrorIs(t, err, ErrInvalidRequest)
	require.NoError(t, os.MkdirAll(m.paths.InstanceSnapshotLatest(meta.Id), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(m.paths.InstanceSnapshotLatest(meta.Id), "incomplete"), []byte("bad-state"), 0600))
	_, err = m.CreateSnapshot(context.Background(), meta.Id, CreateSnapshotRequest{Kind: SnapshotKindStandby})
	require.ErrorIs(t, err, ErrInvalidRequest)
}

func TestMacOSSnapshotRestoreRefusesLiveStates(t *testing.T) {
	for _, state := range []State{StateCreated, StateInitializing, StateRunning, StatePaused, StateShutdown} {
		require.ErrorIs(t, validateMacOSSnapshotRestoreState(&Instance{StoredMetadata: *macOSGuestMetadata(2), State: state}), ErrInvalidState)
	}
	for _, state := range []State{StateStopped, StateStandby} {
		require.NoError(t, validateMacOSSnapshotRestoreState(&Instance{StoredMetadata: *macOSGuestMetadata(2), State: state}))
	}
	require.NoError(t, validateMacOSSnapshotRestoreState(&Instance{State: StatePaused}), "Linux admission is unchanged")
}

func TestMacOSSnapshotSchedulesCaptureStoppedAndStandbyAndRetain(t *testing.T) {
	m, meta := macOSMemorySnapshotFixture(t)
	clock := time.Now().UTC()
	m.now = func() time.Time { return clock }
	ctx := context.Background()
	_, err := m.SetSnapshotSchedule(ctx, meta.Id, SetSnapshotScheduleRequest{Interval: time.Hour, NamePrefix: "mac-backup", Retention: SnapshotScheduleRetention{MaxCount: 1}})
	require.NoError(t, err)
	markSnapshotScheduleDue(t, m, meta.Id)
	require.NoError(t, m.RunSnapshotSchedules(ctx))
	first, err := m.GetSnapshotSchedule(ctx, meta.Id)
	require.NoError(t, err)
	require.NotNil(t, first.LastSnapshotID)
	stopped, err := m.GetSnapshot(ctx, *first.LastSnapshotID)
	require.NoError(t, err)
	require.Equal(t, SnapshotKindStopped, stopped.Kind)

	writeMacOSMemorySnapshotPayload(t, m, meta.DataDir, &meta.StoredMetadata)
	meta.StopGraceful = false
	require.NoError(t, m.saveMetadata(meta))
	clock = clock.Add(time.Hour)
	markSnapshotScheduleDue(t, m, meta.Id)
	require.NoError(t, m.RunSnapshotSchedules(ctx))
	second, err := m.GetSnapshotSchedule(ctx, meta.Id)
	require.NoError(t, err)
	require.NotNil(t, second.LastSnapshotID)
	require.Nil(t, second.LastError)
	memory, err := m.GetSnapshot(ctx, *second.LastSnapshotID)
	require.NoError(t, err)
	require.Equal(t, SnapshotKindStandby, memory.Kind)
	_, err = m.GetSnapshot(ctx, stopped.Id)
	require.ErrorIs(t, err, ErrSnapshotNotFound)
	_, err = m.GetSnapshot(ctx, memory.Id)
	require.NoError(t, err)
	require.NoError(t, m.DeleteSnapshotSchedule(ctx, meta.Id))
}

func TestMacOSSnapshotScheduleRecordsFailureAndAdvancesInterval(t *testing.T) {
	m, meta := macOSMemorySnapshotFixture(t)
	meta.StopGraceful = false
	require.NoError(t, m.saveMetadata(meta))
	ctx := context.Background()
	_, err := m.SetSnapshotSchedule(ctx, meta.Id, SetSnapshotScheduleRequest{Interval: time.Hour, NamePrefix: "mac-backup", Retention: SnapshotScheduleRetention{MaxCount: 1}})
	require.NoError(t, err)
	markSnapshotScheduleDue(t, m, meta.Id)
	require.ErrorIs(t, m.RunSnapshotSchedules(ctx), ErrInvalidState)
	schedule, err := m.GetSnapshotSchedule(ctx, meta.Id)
	require.NoError(t, err)
	require.NotNil(t, schedule.LastError)
	require.Nil(t, schedule.LastSnapshotID)
	require.True(t, schedule.NextRunAt.After(m.now().UTC()))
}

// macOS boots its own disk and aux copies, so restore must not need the template image.
func TestMacOSRestoreDoesNotNeedTemplateImage(t *testing.T) {
	m, meta := macOSMemorySnapshotFixture(t)
	writeMacOSMemorySnapshotPayload(t, m, meta.DataDir, &meta.StoredMetadata)
	m.imageManager = readyFixtureImageManager{name: "deleted"}
	require.NoError(t, m.restoreSnapshotSource(context.Background(), &meta.StoredMetadata))
	inst, err := m.GetInstance(context.Background(), meta.Id)
	require.NoError(t, err)
	require.Equal(t, StateRunning, inst.State)
	require.NoDirExists(t, m.paths.InstanceSnapshotLatest(meta.Id))
}

// A resume that failed before removing its checkpoint leaves a running guest with
// one on disk. A retried restore must remove it rather than return early.
func TestMacOSRestoreRetryRemovesConsumedCheckpoint(t *testing.T) {
	m, meta := macOSMemorySnapshotFixture(t)
	writeMacOSMemorySnapshotPayload(t, m, meta.DataDir, &meta.StoredMetadata)
	require.NoError(t, os.WriteFile(meta.SocketPath, nil, 0600))
	m.storeCachedHypervisorState(meta.Id, hypervisor.StateRunning)

	inst, err := m.RestoreInstance(context.Background(), meta.Id)
	require.NoError(t, err)
	require.Equal(t, StateRunning, inst.State)
	require.False(t, inst.HasSnapshot)
	require.NoDirExists(t, m.paths.InstanceSnapshotLatest(meta.Id))
}

func TestMacOSRestoreFailsWhenCheckpointCannotBeRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	m, meta := macOSMemorySnapshotFixture(t)
	writeMacOSMemorySnapshotPayload(t, m, meta.DataDir, &meta.StoredMetadata)
	snapshots := filepath.Dir(m.paths.InstanceSnapshotLatest(meta.Id))
	require.NoError(t, os.Chmod(snapshots, 0500))
	t.Cleanup(func() { os.Chmod(snapshots, 0700) })

	_, err := m.restoreInstance(context.Background(), meta.Id)
	require.ErrorContains(t, err, "remove consumed snapshot")

	require.NoError(t, os.Chmod(snapshots, 0700))
	m.storeCachedHypervisorState(meta.Id, hypervisor.StateRunning)
	inst, err := m.RestoreInstance(context.Background(), meta.Id)
	require.NoError(t, err)
	require.False(t, inst.HasSnapshot, "the retry removes the consumed checkpoint")
}
