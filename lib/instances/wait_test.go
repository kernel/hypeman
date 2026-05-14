package instances

import (
	"context"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/resources"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubManager is a minimal Manager implementation for unit-testing WaitForState.
type stubManager struct {
	subs        *lifecycleSubscribers
	getInstance func(ctx context.Context, id string) (*Instance, error)
}

func (s *stubManager) SubscribeLifecycleEvents(consumer LifecycleEventConsumer) (<-chan LifecycleEvent, func()) {
	return s.subs.Subscribe(consumer)
}

func (s *stubManager) GetInstance(ctx context.Context, id string) (*Instance, error) {
	if s.getInstance != nil {
		return s.getInstance(ctx, id)
	}
	return nil, ErrNotFound
}

// Unused interface methods — only GetInstance and SubscribeLifecycleEvents are needed.
func (s *stubManager) ListInstances(context.Context, *ListInstancesFilter) ([]Instance, error) {
	return nil, nil
}
func (s *stubManager) ListSnapshots(context.Context, *ListSnapshotsFilter) ([]Snapshot, error) {
	return nil, nil
}
func (s *stubManager) GetSnapshot(context.Context, string) (*Snapshot, error) { return nil, nil }
func (s *stubManager) CreateInstance(context.Context, CreateInstanceRequest) (*Instance, error) {
	return nil, nil
}
func (s *stubManager) CreateSnapshot(context.Context, string, CreateSnapshotRequest) (*Snapshot, error) {
	return nil, nil
}
func (s *stubManager) DeleteInstance(context.Context, string) error { return nil }
func (s *stubManager) DeleteSnapshot(context.Context, string) error { return nil }
func (s *stubManager) ForkInstance(context.Context, string, ForkInstanceRequest) (*Instance, error) {
	return nil, nil
}
func (s *stubManager) ForkSnapshot(context.Context, string, ForkSnapshotRequest) (*Instance, error) {
	return nil, nil
}
func (s *stubManager) StandbyInstance(context.Context, string, StandbyInstanceRequest) (*Instance, error) {
	return nil, nil
}
func (s *stubManager) RestoreInstance(context.Context, string) (*Instance, error) { return nil, nil }
func (s *stubManager) PromoteToTemplate(context.Context, string) (*Instance, error) {
	return nil, nil
}
func (s *stubManager) DemoteTemplate(context.Context, string) (*Instance, error) { return nil, nil }
func (s *stubManager) RestoreSnapshot(context.Context, string, string, RestoreSnapshotRequest) (*Instance, error) {
	return nil, nil
}
func (s *stubManager) StopInstance(context.Context, string) (*Instance, error) { return nil, nil }
func (s *stubManager) StartInstance(context.Context, string, StartInstanceRequest) (*Instance, error) {
	return nil, nil
}
func (s *stubManager) UpdateInstance(context.Context, string, UpdateInstanceRequest) (*Instance, error) {
	return nil, nil
}
func (s *stubManager) StreamInstanceLogs(context.Context, string, int, bool, LogSource) (<-chan string, error) {
	return nil, nil
}
func (s *stubManager) RotateLogs(context.Context, int64, int) error { return nil }
func (s *stubManager) AttachVolume(context.Context, string, string, AttachVolumeRequest) (*Instance, error) {
	return nil, nil
}
func (s *stubManager) DetachVolume(context.Context, string, string) (*Instance, error) {
	return nil, nil
}
func (s *stubManager) ListInstanceAllocations(context.Context) ([]resources.InstanceAllocation, error) {
	return nil, nil
}
func (s *stubManager) ListRunningInstancesInfo(context.Context) ([]resources.InstanceUtilizationInfo, error) {
	return nil, nil
}
func (s *stubManager) SetResourceValidator(ResourceValidator) {}
func (s *stubManager) GetVsockDialer(context.Context, string) (hypervisor.VsockDialer, error) {
	return nil, nil
}

func TestWaitForState_SubscriptionDelivers(t *testing.T) {
	t.Parallel()
	subs := newLifecycleSubscribers()
	mgr := &stubManager{subs: subs}

	inst := &Instance{}
	inst.Id = "test-sub"
	inst.State = StateInitializing

	// Simulate a state change via subscription after 100ms.
	go func() {
		time.Sleep(100 * time.Millisecond)
		subs.Notify(context.Background(), LifecycleEvent{
			Action:     LifecycleEventStart,
			InstanceID: "test-sub",
			Instance:   &Instance{State: StateRunning},
		})
	}()

	start := time.Now()
	result, err := WaitForState(context.Background(), mgr, inst, StateRunning, 30*time.Second)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, StateRunning, result.State)
	assert.False(t, result.TimedOut)
	assert.Less(t, elapsed, 2*time.Second, "subscription should deliver well before the 5s poll")
}

func TestWaitForState_ChannelClosedOnDelete(t *testing.T) {
	t.Parallel()
	subs := newLifecycleSubscribers()
	mgr := &stubManager{subs: subs}

	inst := &Instance{}
	inst.Id = "test-close"
	inst.State = StateInitializing

	// Simulate instance deletion (close all subscriber channels).
	go func() {
		time.Sleep(100 * time.Millisecond)
		subs.Notify(context.Background(), LifecycleEvent{
			Action:     LifecycleEventDelete,
			InstanceID: "test-close",
		})
	}()

	start := time.Now()
	result, err := WaitForState(context.Background(), mgr, inst, StateRunning, 30*time.Second)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrNotFound)
	assert.Nil(t, result)
	assert.Less(t, elapsed, 2*time.Second, "closed channel should be detected immediately")
}

func TestWaitForState_TerminalViaSubscription(t *testing.T) {
	t.Parallel()
	subs := newLifecycleSubscribers()
	mgr := &stubManager{subs: subs}

	inst := &Instance{}
	inst.Id = "test-terminal-sub"
	inst.State = StateInitializing

	go func() {
		time.Sleep(100 * time.Millisecond)
		subs.Notify(context.Background(), LifecycleEvent{
			Action:     LifecycleEventStop,
			InstanceID: "test-terminal-sub",
			Instance:   &Instance{State: StateStopped},
		})
	}()

	result, err := WaitForState(context.Background(), mgr, inst, StateRunning, 30*time.Second)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, StateStopped, result.State)
	assert.False(t, result.TimedOut)
}

func TestWaitForState_ShutdownIsTerminal(t *testing.T) {
	t.Parallel()
	subs := newLifecycleSubscribers()
	mgr := &stubManager{subs: subs}

	inst := &Instance{}
	inst.Id = "test-shutdown"
	inst.State = StateInitializing

	go func() {
		time.Sleep(100 * time.Millisecond)
		subs.Notify(context.Background(), LifecycleEvent{
			Action:     LifecycleEventStop,
			InstanceID: "test-shutdown",
			Instance:   &Instance{State: StateShutdown},
		})
	}()

	start := time.Now()
	result, err := WaitForState(context.Background(), mgr, inst, StateRunning, 30*time.Second)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, StateShutdown, result.State)
	assert.False(t, result.TimedOut)
	assert.Less(t, elapsed, 2*time.Second, "shutdown should be detected as terminal immediately")
}

func TestWaitForState_PausedIsTerminal(t *testing.T) {
	t.Parallel()
	subs := newLifecycleSubscribers()
	mgr := &stubManager{subs: subs}

	inst := &Instance{}
	inst.Id = "test-paused"
	inst.State = StateInitializing

	go func() {
		time.Sleep(100 * time.Millisecond)
		subs.Notify(context.Background(), LifecycleEvent{
			Action:     LifecycleEventStandby,
			InstanceID: "test-paused",
			Instance:   &Instance{State: StatePaused},
		})
	}()

	start := time.Now()
	result, err := WaitForState(context.Background(), mgr, inst, StateRunning, 30*time.Second)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, StatePaused, result.State)
	assert.False(t, result.TimedOut)
	assert.Less(t, elapsed, 2*time.Second, "paused should be detected as terminal immediately")
}

func TestWaitForState_IgnoresEventsForOtherInstances(t *testing.T) {
	t.Parallel()
	subs := newLifecycleSubscribers()
	mgr := &stubManager{subs: subs}

	inst := &Instance{}
	inst.Id = "target-instance"
	inst.State = StateInitializing

	go func() {
		time.Sleep(50 * time.Millisecond)
		subs.Notify(context.Background(), LifecycleEvent{
			Action:     LifecycleEventStart,
			InstanceID: "other-instance",
			Instance:   &Instance{State: StateRunning},
		})
		time.Sleep(50 * time.Millisecond)
		subs.Notify(context.Background(), LifecycleEvent{
			Action:     LifecycleEventStart,
			InstanceID: "target-instance",
			Instance:   &Instance{State: StateRunning},
		})
	}()

	result, err := WaitForState(context.Background(), mgr, inst, StateRunning, 30*time.Second)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, StateRunning, result.State)
}

func TestWaitForState_IgnoresNilInstancePayloadAndUsesPollingFallback(t *testing.T) {
	t.Parallel()
	subs := newLifecycleSubscribers()
	mgr := &stubManager{
		subs: subs,
		getInstance: func(ctx context.Context, id string) (*Instance, error) {
			return &Instance{
				StoredMetadata: StoredMetadata{Id: id},
				State:          StateRunning,
			}, nil
		},
	}

	inst := &Instance{}
	inst.Id = "test-nil-event"
	inst.State = StateInitializing

	go func() {
		time.Sleep(100 * time.Millisecond)
		subs.Notify(context.Background(), LifecycleEvent{
			Action:     LifecycleEventStart,
			InstanceID: "test-nil-event",
		})
	}()

	start := time.Now()
	result, err := WaitForState(context.Background(), mgr, inst, StateRunning, 6*time.Second)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, StateRunning, result.State)
	assert.GreaterOrEqual(t, elapsed, WaitForStatePollInterval)
}
