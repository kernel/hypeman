package instances

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	restartpolicy "github.com/kernel/hypeman/lib/restart-policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const lifecycleNoopHypervisorType hypervisor.Type = "lifecycle-noop-test"

var lifecycleNoopHypervisorStates sync.Map

func init() {
	hypervisor.RegisterClientFactory(lifecycleNoopHypervisorType, func(socketPath string) (hypervisor.Hypervisor, error) {
		state, ok := lifecycleNoopHypervisorStates.Load(socketPath)
		if !ok {
			return nil, errors.New("missing fake hypervisor state")
		}
		return lifecycleNoopHypervisor{state: state.(hypervisor.VMState)}, nil
	})
}

type lifecycleNoopHypervisor struct {
	state hypervisor.VMState
}

func (h lifecycleNoopHypervisor) DeleteVM(context.Context) error { return nil }
func (h lifecycleNoopHypervisor) Shutdown(context.Context) error { return nil }
func (h lifecycleNoopHypervisor) GetVMInfo(context.Context) (*hypervisor.VMInfo, error) {
	return &hypervisor.VMInfo{State: h.state}, nil
}
func (h lifecycleNoopHypervisor) Pause(context.Context) error  { return nil }
func (h lifecycleNoopHypervisor) Resume(context.Context) error { return nil }
func (h lifecycleNoopHypervisor) Snapshot(context.Context, string) error {
	return nil
}
func (h lifecycleNoopHypervisor) ResizeMemory(context.Context, int64) error {
	return nil
}
func (h lifecycleNoopHypervisor) ResizeMemoryAndWait(context.Context, int64, time.Duration) error {
	return nil
}
func (h lifecycleNoopHypervisor) SetTargetGuestMemoryBytes(context.Context, int64) error {
	return nil
}
func (h lifecycleNoopHypervisor) GetTargetGuestMemoryBytes(context.Context) (int64, error) {
	return 0, nil
}
func (h lifecycleNoopHypervisor) Capabilities() hypervisor.Capabilities {
	return hypervisor.Capabilities{}
}

func TestLifecycleNoopTransitionsReturnCurrentInstanceWithoutEvent(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		name   string
		state  State
		action func(context.Context, *manager, string) (*Instance, error)
	}{
		{
			name:  "restore running",
			state: StateRunning,
			action: func(ctx context.Context, m *manager, id string) (*Instance, error) {
				return m.RestoreInstance(ctx, id)
			},
		},
		{
			name:  "restore initializing",
			state: StateInitializing,
			action: func(ctx context.Context, m *manager, id string) (*Instance, error) {
				return m.RestoreInstance(ctx, id)
			},
		},
		{
			name:  "start running without overrides",
			state: StateRunning,
			action: func(ctx context.Context, m *manager, id string) (*Instance, error) {
				return m.StartInstance(ctx, id, StartInstanceRequest{})
			},
		},
		{
			name:  "start initializing without overrides",
			state: StateInitializing,
			action: func(ctx context.Context, m *manager, id string) (*Instance, error) {
				return m.StartInstance(ctx, id, StartInstanceRequest{})
			},
		},
		{
			name:  "standby already standby without options",
			state: StateStandby,
			action: func(ctx context.Context, m *manager, id string) (*Instance, error) {
				return m.StandbyInstance(ctx, id, StandbyInstanceRequest{})
			},
		},
		{
			name:  "stop already stopped",
			state: StateStopped,
			action: func(ctx context.Context, m *manager, id string) (*Instance, error) {
				return m.StopInstance(ctx, id)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, id := newLifecycleNoopManagerWithInstance(t, tt.state, now)
			events, cancel := m.SubscribeLifecycleEvents(LifecycleEventConsumerWaitForState)
			defer cancel()

			inst, err := tt.action(context.Background(), m, id)
			require.NoError(t, err)
			require.NotNil(t, inst)
			assert.Equal(t, tt.state, inst.State)
			assertNoLifecycleEvent(t, events)
		})
	}
}

func TestLifecycleNoopStartWithOverridesStillRejectsActiveInstance(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateRunning, time.Now().UTC())
	events, cancel := m.SubscribeLifecycleEvents(LifecycleEventConsumerWaitForState)
	defer cancel()

	_, err := m.StartInstance(context.Background(), id, StartInstanceRequest{Cmd: []string{"echo", "hello"}})
	require.ErrorIs(t, err, ErrInvalidState)
	assertNoLifecycleEvent(t, events)
}

func TestLifecycleNoopStandbyWithOptionsStillRejectsStandbyInstance(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateStandby, time.Now().UTC())
	events, cancel := m.SubscribeLifecycleEvents(LifecycleEventConsumerWaitForState)
	defer cancel()

	delay := time.Second
	_, err := m.StandbyInstance(context.Background(), id, StandbyInstanceRequest{CompressionDelay: &delay})
	require.ErrorIs(t, err, ErrInvalidState)
	assertNoLifecycleEvent(t, events)
}

func TestDeleteContinuesWhenVGPUReleaseFails(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateStopped, time.Now().UTC())
	m.orphanedVGPURetryDelay = time.Millisecond
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.GPUProfile = "NVIDIA L40S-2Q"
	meta.GPUFramework = devices.VGPUFramework("future-framework")
	meta.GPUDevicePath = "/sys/bus/pci/devices/0000:82:00.4"
	require.NoError(t, m.saveMetadata(meta))

	// A failed release is logged and the delete continues, matching the
	// pre-refactor contract; the leaked assignment is recovered by the
	// background retry or startup reconciliation.
	require.NoError(t, m.DeleteInstance(context.Background(), id))
	waitForOrphanQueueEmpty(t, m)

	_, err = m.loadMetadata(id)
	require.Error(t, err, "instance data must be deleted despite the failed release")
}

func TestDeletePersistsVGPUReleaseBeforeTeardown(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateStopped, time.Now().UTC())
	var persisted *metadata
	deviceManager := &recordingDeviceManager{
		onMarkDetached: func() {
			var err error
			persisted, err = m.loadMetadata(id)
			require.NoError(t, err)
		},
	}
	m.deviceManager = deviceManager
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.RestartPolicy = &restartpolicy.Policy{Policy: restartpolicy.PolicyAlways}
	meta.GPUProfile = "NVIDIA L40S-2Q"
	meta.GPUDevicePath = "/sys/bus/mdev/devices/test-mdev"
	meta.GPUMdevUUID = "test-mdev"
	meta.Devices = []string{"dev-1"}
	require.NoError(t, m.saveMetadata(meta))

	require.NoError(t, m.DeleteInstance(context.Background(), id))
	require.NotNil(t, persisted)
	assert.Empty(t, persisted.GPUDevicePath)
	assert.Empty(t, persisted.GPUMdevUUID)
	assert.Equal(t, "NVIDIA L40S-2Q", persisted.GPUProfile)
	assert.Equal(t, restartpolicy.BlockedReasonManualStop, persisted.RestartStatus.BlockedReason)
}

func TestDeleteReleasesRetainedCreateStub(t *testing.T) {
	p := paths.New(t.TempDir())
	var destroyed []devices.VGPUAssignment
	m := &manager{
		paths:           p,
		instanceLocks:   sync.Map{},
		bootMarkerScans: sync.Map{},
		now:             time.Now,
		lifecycleEvents: newLifecycleSubscribers(),
		destroyVGPU: func(_ context.Context, assignment devices.VGPUAssignment) error {
			destroyed = append(destroyed, assignment)
			return nil
		},
	}
	const id = "retained-stub"
	require.NoError(t, m.ensureDirectories(id))
	assignedAt := time.Now().UTC()
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:            id,
		GPUFramework:  devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
		GPUAssignedAt: &assignedAt,
	}}))

	require.NoError(t, m.DeleteInstance(context.Background(), id))

	require.Len(t, destroyed, 1)
	assert.Equal(t, "/sys/bus/pci/devices/0000:82:00.4", destroyed[0].DevicePath)
	assert.Equal(t, id, destroyed[0].InstanceID)
	_, err := m.loadMetadata(id)
	require.Error(t, err, "retained stub must be fully deleted")
}

func TestDeleteDropsStaleVGPUClaimedByLiveInstance(t *testing.T) {
	now := time.Now().UTC()
	m, id := newLifecycleNoopManagerWithInstance(t, StateStopped, now)
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.GPUProfile = "NVIDIA L40S-2Q"
	meta.GPUFramework = devices.VGPUFrameworkVendorVFIO
	meta.GPUDevicePath = "/sys/bus/pci/devices/0000:82:00.4"
	require.NoError(t, m.saveMetadata(meta))

	claimantID := "inst-live-claimant"
	require.NoError(t, m.ensureDirectories(claimantID))
	pid := os.Getpid()
	// Bind under /tmp: a t.TempDir()-derived path exceeds the macOS AF_UNIX
	// path limit.
	socketDir, err := os.MkdirTemp("/tmp", "hypeman-claimant-socket-")
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = os.RemoveAll(socketDir)
	})
	socketPath := filepath.Join(socketDir, "noop.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:                        claimantID,
		Name:                      claimantID,
		Image:                     "test-image",
		CreatedAt:                 now,
		HypervisorType:            lifecycleNoopHypervisorType,
		HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &pid},
		SocketPath:                socketPath,
		DataDir:                   m.paths.InstanceDir(claimantID),
		GPUProfile:                "NVIDIA L40S-2Q",
		GPUFramework:              devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath:             "/sys/bus/pci/devices/0000:82:00.4",
	}}))

	require.NoError(t, m.DeleteInstance(context.Background(), id))

	_, err = m.loadMetadata(id)
	require.Error(t, err, "deleted instance metadata should be gone")
	claimant, err := m.loadMetadata(claimantID)
	require.NoError(t, err)
	assert.Equal(t, "/sys/bus/pci/devices/0000:82:00.4", claimant.GPUDevicePath, "live claimant keeps its assignment")
}

func TestDeleteContinuesTeardownAfterFailedVGPURelease(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateStopped, time.Now().UTC())
	m.orphanedVGPURetryDelay = time.Millisecond
	deviceManager := &recordingDeviceManager{}
	m.deviceManager = deviceManager
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.GPUProfile = "NVIDIA L40S-2Q"
	meta.GPUFramework = devices.VGPUFramework("future-framework")
	meta.GPUDevicePath = "/sys/bus/pci/devices/0000:82:00.4"
	meta.Devices = []string{"dev-1"}
	require.NoError(t, m.saveMetadata(meta))

	// The failed release must not block the rest of the teardown: devices
	// are detached and the instance is fully deleted.
	require.NoError(t, m.DeleteInstance(context.Background(), id))
	waitForOrphanQueueEmpty(t, m)
	assert.Equal(t, []string{"dev-1"}, deviceManager.detached)

	_, err = m.loadMetadata(id)
	require.Error(t, err, "instance data must be deleted despite the failed release")
}

// A stale release during start must be persisted immediately: if start fails
// later (here at vGPU recreation on a host without VFs), the on-disk metadata
// must no longer point at the already-released device.
func TestStartPersistsStaleVGPUReleaseImmediately(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateStopped, time.Now().UTC())
	m.imageManager = readyFixtureImageManager{name: "test-image"}
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.GPUProfile = "NVIDIA L40S-2Q"
	meta.HypervisorType = hypervisor.TypeQEMU
	meta.GPUFramework = devices.VGPUFrameworkNone
	meta.GPUDevicePath = "/sys/bus/pci/devices/0000:82:00.4"
	require.NoError(t, m.saveMetadata(meta))

	_, err = m.StartInstance(context.Background(), id, StartInstanceRequest{})
	require.Error(t, err)

	stored, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Empty(t, stored.GPUDevicePath, "released assignment should be persisted despite the failed start")
	assert.Equal(t, "NVIDIA L40S-2Q", stored.GPUProfile, "profile is kept for the next start")
}

func TestStartRejectsVGPURetentionRecord(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateStopped, time.Now().UTC())
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.GPUProfile = "NVIDIA L40S-2Q"
	meta.GPUFramework = devices.VGPUFrameworkNone
	meta.GPUDevicePath = "/sys/bus/pci/devices/0000:82:00.4"
	meta.GPURetainedForCleanup = true
	require.NoError(t, m.saveMetadata(meta))

	_, err = m.StartInstance(context.Background(), id, StartInstanceRequest{})
	require.ErrorIs(t, err, ErrInvalidState)
	require.ErrorContains(t, err, "delete it to release the assignment")

	stored, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Equal(t, "/sys/bus/pci/devices/0000:82:00.4", stored.GPUDevicePath)
}

func TestStopStoppedInstanceReleasesRetainedVGPU(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateStopped, time.Now().UTC())
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.GPUProfile = "NVIDIA L40S-2Q"
	meta.GPUFramework = devices.VGPUFrameworkNone
	meta.GPUDevicePath = "/sys/bus/pci/devices/0000:82:00.4"
	require.NoError(t, m.saveMetadata(meta))

	inst, err := m.StopInstance(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, inst)
	assert.Equal(t, StateStopped, inst.State)

	stored, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Empty(t, stored.GPUDevicePath)
}

func TestStopStoppedInstanceLeavesRetentionStubForDelete(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateStopped, time.Now().UTC())
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.GPUProfile = "NVIDIA L40S-2Q"
	meta.GPUFramework = devices.VGPUFrameworkNone
	meta.GPUDevicePath = "/sys/bus/pci/devices/0000:82:00.4"
	meta.GPURetainedForCleanup = true
	require.NoError(t, m.saveMetadata(meta))

	inst, err := m.StopInstance(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, inst)

	stored, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Equal(t, "/sys/bus/pci/devices/0000:82:00.4", stored.GPUDevicePath)
	assert.True(t, stored.GPURetainedForCleanup)
}

func TestStopStoppedInstanceVGPUReleaseFailureRemainsNoop(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateStopped, time.Now().UTC())
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.GPUProfile = "NVIDIA L40S-2Q"
	meta.GPUFramework = devices.VGPUFramework("future-framework")
	meta.GPUDevicePath = "/sys/bus/pci/devices/0000:82:00.4"
	require.NoError(t, m.saveMetadata(meta))

	inst, err := m.StopInstance(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, inst)
	assert.Equal(t, StateStopped, inst.State)

	stored, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Equal(t, devices.VGPUFramework("future-framework"), stored.GPUFramework)
	assert.Equal(t, "/sys/bus/pci/devices/0000:82:00.4", stored.GPUDevicePath)
}

// recordingDeviceManager is a devices.Manager stub that records passthrough
// teardown calls. Only the methods delete exercises are implemented.
type recordingDeviceManager struct {
	devices.Manager
	detached       []string
	unbound        []string
	onMarkDetached func()
}

func (m *recordingDeviceManager) MarkDetached(ctx context.Context, deviceID string) error {
	m.detached = append(m.detached, deviceID)
	if m.onMarkDetached != nil {
		m.onMarkDetached()
	}
	return nil
}

func (m *recordingDeviceManager) UnbindFromVFIO(ctx context.Context, id string) error {
	m.unbound = append(m.unbound, id)
	return nil
}

func TestLifecycleNoopStandbyRejectsVendorVFIOVGPU(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateRunning, time.Now().UTC())
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.GPUProfile = "NVIDIA L40S-2Q"
	meta.GPUFramework = devices.VGPUFrameworkVendorVFIO
	meta.GPUDevicePath = "/sys/bus/pci/devices/0000:82:00.4"
	require.NoError(t, m.saveMetadata(meta))

	_, err = m.StandbyInstance(context.Background(), id, StandbyInstanceRequest{})
	require.ErrorIs(t, err, ErrInvalidState)
	assert.ErrorContains(t, err, "standby is not supported for instances with vGPU attached")
}

func newLifecycleNoopManagerWithInstance(t *testing.T, state State, now time.Time) (*manager, string) {
	t.Helper()

	p := paths.New(t.TempDir())
	m := &manager{
		paths:           p,
		instanceLocks:   sync.Map{},
		bootMarkerScans: sync.Map{},
		now: func() time.Time {
			return now
		},
		lifecycleEvents: newLifecycleSubscribers(),
	}

	id := "inst-" + string(state)
	require.NoError(t, m.ensureDirectories(id))

	stored := StoredMetadata{
		Id:             id,
		Name:           id,
		Image:          "test-image",
		CreatedAt:      now,
		HypervisorType: lifecycleNoopHypervisorType,
		SocketPath:     p.InstanceSocket(id, "noop.sock"),
		DataDir:        p.InstanceDir(id),
	}

	switch state {
	case StateRunning:
		stored.ProgramStartedAt = &now
		stored.GuestAgentReadyAt = &now
		writeLifecycleNoopSocket(t, stored.SocketPath, hypervisor.StateRunning)
	case StateInitializing:
		writeLifecycleNoopSocket(t, stored.SocketPath, hypervisor.StateRunning)
	case StateStandby:
		writeLifecycleNoopSnapshot(t, p, id)
	}

	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: stored}))
	t.Cleanup(func() {
		lifecycleNoopHypervisorStates.Delete(stored.SocketPath)
	})
	return m, id
}

func writeLifecycleNoopSocket(t *testing.T, socketPath string, state hypervisor.VMState) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(socketPath), 0o755))
	require.NoError(t, os.WriteFile(socketPath, []byte("fake socket"), 0o644))
	lifecycleNoopHypervisorStates.Store(socketPath, state)
}

func writeLifecycleNoopSnapshot(t *testing.T, p *paths.Paths, id string) {
	t.Helper()
	snapshotDir := p.InstanceSnapshotLatest(id)
	require.NoError(t, os.MkdirAll(snapshotDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(snapshotDir, "memory"), []byte("snapshot"), 0o644))
}

func assertNoLifecycleEvent(t *testing.T, events <-chan LifecycleEvent) {
	t.Helper()
	select {
	case event := <-events:
		t.Fatalf("unexpected lifecycle event: %+v", event)
	default:
	}
}
