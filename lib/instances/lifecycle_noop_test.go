package instances

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
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
func (h lifecycleNoopHypervisor) Snapshot(context.Context, string, hypervisor.SnapshotOptions) error {
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
				return m.StopInstance(ctx, id, nil)
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
