package autostandby

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeInstanceStore struct {
	mu               sync.Mutex
	instances        []Instance
	standbyIDs       []string
	persistedRuntime map[string]*Runtime
	events           chan InstanceEvent
	standbyErr       error
	listErr          error
	setRuntimeErr    error
	standbyStarted   chan string
	standbyRelease   chan struct{}
}

func newFakeInstanceStore(instances []Instance) *fakeInstanceStore {
	return &fakeInstanceStore{
		instances:        append([]Instance(nil), instances...),
		persistedRuntime: make(map[string]*Runtime),
		events:           make(chan InstanceEvent, 16),
	}
}

func (f *fakeInstanceStore) ListInstances(context.Context) ([]Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]Instance, 0, len(f.instances))
	for _, inst := range f.instances {
		out = append(out, cloneInstance(inst))
	}
	return out, nil
}

func (f *fakeInstanceStore) StandbyInstance(_ context.Context, id string) error {
	if f.standbyStarted != nil {
		f.standbyStarted <- id
	}
	if f.standbyRelease != nil {
		<-f.standbyRelease
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.standbyIDs = append(f.standbyIDs, id)
	return f.standbyErr
}

func (f *fakeInstanceStore) standbyCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.standbyIDs...)
}

func (f *fakeInstanceStore) SetRuntime(_ context.Context, id string, runtime *Runtime) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setRuntimeErr != nil {
		return f.setRuntimeErr
	}
	f.persistedRuntime[id] = cloneRuntime(runtime)
	for i := range f.instances {
		if f.instances[i].ID == id {
			f.instances[i].Runtime = cloneRuntime(runtime)
		}
	}
	return nil
}

func (f *fakeInstanceStore) SubscribeInstanceEvents() (<-chan InstanceEvent, func(), error) {
	return f.events, func() {}, nil
}

type fakeConnectionSource struct {
	connections []Connection
	listErr     error
	openErr     error
}

func (f *fakeConnectionSource) ListConnections(context.Context) ([]Connection, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]Connection(nil), f.connections...), nil
}

func (f *fakeConnectionSource) OpenStream(context.Context) (ConnectionStream, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &fakeConnectionStream{
		events: make(chan ConnectionEvent),
		errs:   make(chan error),
	}, nil
}

type fakeConnectionStream struct {
	events chan ConnectionEvent
	errs   chan error
}

func (f *fakeConnectionStream) Events() <-chan ConnectionEvent { return f.events }

func (f *fakeConnectionStream) Errors() <-chan error { return f.errs }

func (f *fakeConnectionStream) Close() error {
	select {
	case <-f.events:
	default:
	}
	return nil
}

func TestStartupResyncClearsPersistedIdleWhenCurrentConnectionsExist(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 0, 0, 0, time.UTC)
	lastInbound := idleSince.Add(-time.Minute)
	store := newFakeInstanceStore([]Instance{{
		ID:             "inst-active",
		Name:           "inst-active",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.10",
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: "5m"},
		Runtime: &Runtime{
			IdleSince:             &idleSince,
			LastInboundActivityAt: &lastInbound,
		},
	}})
	source := &fakeConnectionSource{connections: []Connection{{
		OriginalSourceIP:        mustAddr("1.2.3.4"),
		OriginalSourcePort:      51234,
		OriginalDestinationIP:   mustAddr("192.168.100.10"),
		OriginalDestinationPort: 8080,
		TCPState:                TCPStateEstablished,
	}}}
	now := time.Date(2026, 4, 6, 11, 0, 0, 0, time.UTC)
	controller := NewController(store, source, ControllerOptions{
		Now: func() time.Time { return now },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	status := controller.Describe(store.instances[0])
	require.Equal(t, StatusActive, status.Status)
	require.Nil(t, status.IdleSince)
	require.NotNil(t, store.persistedRuntime["inst-active"])
	require.Nil(t, store.persistedRuntime["inst-active"].IdleSince)
}

func TestStartupResyncResumesPersistedIdleCountdown(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	store := newFakeInstanceStore([]Instance{{
		ID:             "inst-idle",
		Name:           "inst-idle",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.20",
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: "10m"},
		Runtime: &Runtime{
			IdleSince: &idleSince,
		},
	}})
	now := time.Date(2026, 4, 6, 11, 0, 0, 0, time.UTC)
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return now },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	status := controller.Describe(store.instances[0])
	require.Equal(t, StatusIdleCountdown, status.Status)
	require.NotNil(t, status.NextStandbyAt)
	assert.Equal(t, idleSince.Add(10*time.Minute), *status.NextStandbyAt)
}

func TestPeriodicSnapshotSyncRefreshesTrackedState(t *testing.T) {
	t.Parallel()

	store := newFakeInstanceStore([]Instance{{
		ID:             "inst-periodic",
		Name:           "inst-periodic",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.21",
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: "10m"},
	}})
	source := &fakeConnectionSource{}
	now := time.Date(2026, 4, 6, 11, 0, 0, 0, time.UTC)
	controller := NewController(store, source, ControllerOptions{
		Now: func() time.Time { return now },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	status := controller.Describe(store.instances[0])
	require.Equal(t, StatusIdleCountdown, status.Status)

	source.connections = []Connection{{
		OriginalSourceIP:        mustAddr("1.2.3.4"),
		OriginalSourcePort:      51235,
		OriginalDestinationIP:   mustAddr("192.168.100.21"),
		OriginalDestinationPort: 8080,
		TCPState:                TCPStateEstablished,
	}}
	now = now.Add(time.Minute)

	require.NoError(t, controller.periodicSnapshotSync(context.Background()))

	status = controller.Describe(store.instances[0])
	require.Equal(t, StatusActive, status.Status)
	require.Equal(t, 1, status.ActiveInboundCount)
}

func TestInstanceEventClearsPersistedRuntimeWhenInstanceBecomesIneligible(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	lastInbound := idleSince.Add(-time.Minute)
	store := newFakeInstanceStore([]Instance{{
		ID:             "inst-ineligible",
		Name:           "inst-ineligible",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.22",
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: "10m"},
		Runtime: &Runtime{
			IdleSince:             &idleSince,
			LastInboundActivityAt: &lastInbound,
		},
	}})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return idleSince.Add(30 * time.Second) },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	require.NoError(t, controller.handleInstanceEvent(context.Background(), InstanceEvent{
		Action:     InstanceEventUpdate,
		InstanceID: "inst-ineligible",
		Instance: &Instance{
			ID:             "inst-ineligible",
			Name:           "inst-ineligible",
			State:          StateRunning,
			NetworkEnabled: false,
			IP:             "192.168.100.22",
			AutoStandby:    &Policy{Enabled: true, IdleTimeout: "10m"},
		},
	}))

	runtime, ok := store.persistedRuntime["inst-ineligible"]
	require.True(t, ok)
	require.Nil(t, runtime)
}

func TestConnectionEventsClearIdleAndStartCountdown(t *testing.T) {
	t.Parallel()

	store := newFakeInstanceStore([]Instance{{
		ID:             "inst-1",
		Name:           "inst-1",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.30",
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: "1m"},
	}})
	now := time.Date(2026, 4, 6, 11, 0, 0, 0, time.UTC)
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return now },
	})
	require.NoError(t, controller.startupResync(context.Background()))

	newEvent := ConnectionEvent{
		Type: ConnectionEventNew,
		Connection: Connection{
			OriginalSourceIP:        mustAddr("1.2.3.4"),
			OriginalSourcePort:      50000,
			OriginalDestinationIP:   mustAddr("192.168.100.30"),
			OriginalDestinationPort: 8080,
			TCPState:                TCPStateEstablished,
		},
		ObservedAt: now.Add(5 * time.Second),
	}
	controller.handleConnectionEvent(context.Background(), newEvent)

	status := controller.Describe(store.instances[0])
	require.Equal(t, StatusActive, status.Status)
	require.Nil(t, status.IdleSince)

	destroyEvent := newEvent
	destroyEvent.Type = ConnectionEventDestroy
	destroyEvent.ObservedAt = now.Add(10 * time.Second)
	controller.handleConnectionEvent(context.Background(), destroyEvent)

	status = controller.Describe(store.instances[0])
	require.Equal(t, StatusIdleCountdown, status.Status)
	require.NotNil(t, status.IdleSince)
}

func TestSynSentConnectionKeepsInstanceActive(t *testing.T) {
	t.Parallel()

	store := newFakeInstanceStore([]Instance{{
		ID:             "inst-synsent",
		Name:           "inst-synsent",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.33",
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: "1m"},
	}})
	now := time.Date(2026, 4, 6, 11, 0, 0, 0, time.UTC)
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return now },
	})
	require.NoError(t, controller.startupResync(context.Background()))

	// A half-open handshake against a slow guest arrives as NEW in SYN_SENT.
	controller.handleConnectionEvent(context.Background(), ConnectionEvent{
		Type: ConnectionEventNew,
		Connection: Connection{
			OriginalSourceIP:        mustAddr("1.2.3.4"),
			OriginalSourcePort:      50005,
			OriginalDestinationIP:   mustAddr("192.168.100.33"),
			OriginalDestinationPort: 8080,
			TCPState:                TCPStateSynSent,
		},
		ObservedAt: now.Add(5 * time.Second),
	})

	status := controller.Describe(store.instances[0])
	require.Equal(t, StatusActive, status.Status)
	require.Equal(t, 1, status.ActiveInboundCount)
	require.Nil(t, status.IdleSince)
}

func TestSnapshotCountsSynSentConnection(t *testing.T) {
	t.Parallel()

	store := newFakeInstanceStore([]Instance{{
		ID:             "inst-synsent-snap",
		Name:           "inst-synsent-snap",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.34",
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: "1m"},
	}})
	source := &fakeConnectionSource{connections: []Connection{{
		OriginalSourceIP:        mustAddr("1.2.3.4"),
		OriginalSourcePort:      50006,
		OriginalDestinationIP:   mustAddr("192.168.100.34"),
		OriginalDestinationPort: 8080,
		TCPState:                TCPStateSynSent,
	}}}
	controller := NewController(store, source, ControllerOptions{
		Now: func() time.Time { return time.Date(2026, 4, 6, 11, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, controller.startupResync(context.Background()))

	status := controller.Describe(store.instances[0])
	require.Equal(t, StatusActive, status.Status)
	require.Equal(t, 1, status.ActiveInboundCount)
}

func TestConnectionUpdateWithInactiveTCPStateStartsCountdown(t *testing.T) {
	t.Parallel()

	store := newFakeInstanceStore([]Instance{{
		ID:             "inst-update",
		Name:           "inst-update",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.31",
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: "1m"},
	}})
	now := time.Date(2026, 4, 6, 11, 0, 0, 0, time.UTC)
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return now },
	})
	require.NoError(t, controller.startupResync(context.Background()))

	event := ConnectionEvent{
		Type: ConnectionEventNew,
		Connection: Connection{
			OriginalSourceIP:        mustAddr("1.2.3.4"),
			OriginalSourcePort:      50001,
			OriginalDestinationIP:   mustAddr("192.168.100.31"),
			OriginalDestinationPort: 8080,
			TCPState:                TCPStateEstablished,
		},
		ObservedAt: now.Add(5 * time.Second),
	}
	controller.handleConnectionEvent(context.Background(), event)

	event.ObservedAt = now.Add(10 * time.Second)
	event.Connection.TCPState = TCPStateTimeWait
	controller.handleConnectionEvent(context.Background(), event)

	status := controller.Describe(store.instances[0])
	require.Equal(t, StatusIdleCountdown, status.Status)
	require.NotNil(t, status.IdleSince)
}

func TestActiveReconcileStartsCountdownForStartupSeededConnections(t *testing.T) {
	t.Parallel()

	idleTimeout := 30 * time.Second
	store := newFakeInstanceStore([]Instance{{
		ID:             "inst-reconcile",
		Name:           "inst-reconcile",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.32",
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: idleTimeout.String()},
	}})
	source := &fakeConnectionSource{connections: []Connection{{
		OriginalSourceIP:        mustAddr("1.2.3.4"),
		OriginalSourcePort:      50002,
		OriginalDestinationIP:   mustAddr("192.168.100.32"),
		OriginalDestinationPort: 8080,
		TCPState:                TCPStateEstablished,
	}}}
	now := time.Date(2026, 4, 6, 11, 0, 0, 0, time.UTC)
	controller := NewController(store, source, ControllerOptions{
		Now:            func() time.Time { return now },
		ReconcileDelay: time.Second,
	})
	require.NoError(t, controller.startupResync(context.Background()))

	status := controller.Describe(store.instances[0])
	require.Equal(t, StatusActive, status.Status)

	source.connections = nil
	now = now.Add(5 * time.Second)
	controller.handleActiveReconcile(context.Background(), "inst-reconcile")

	status = controller.Describe(store.instances[0])
	require.Equal(t, StatusIdleCountdown, status.Status)
	require.NotNil(t, status.IdleSince)
	require.NotNil(t, status.NextStandbyAt)
}

func TestDuplicateDestroyDoesNotGoNegative(t *testing.T) {
	t.Parallel()

	store := newFakeInstanceStore([]Instance{{
		ID:             "inst-dup",
		Name:           "inst-dup",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.40",
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: "1m"},
	}})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: time.Now,
	})
	require.NoError(t, controller.startupResync(context.Background()))

	event := ConnectionEvent{
		Type: ConnectionEventDestroy,
		Connection: Connection{
			OriginalSourceIP:        mustAddr("1.2.3.4"),
			OriginalSourcePort:      50000,
			OriginalDestinationIP:   mustAddr("192.168.100.40"),
			OriginalDestinationPort: 8080,
		},
		ObservedAt: time.Now().UTC(),
	}
	controller.handleConnectionEvent(context.Background(), event)
	controller.handleConnectionEvent(context.Background(), event)

	status := controller.Describe(store.instances[0])
	require.Equal(t, 0, status.ActiveInboundCount)
}

func TestStatusReportsObserverError(t *testing.T) {
	t.Parallel()

	store := newFakeInstanceStore([]Instance{{
		ID:             "inst-err",
		Name:           "inst-err",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.50",
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: "1m"},
	}})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{})
	controller.setObserverError(errors.New("boom"))

	status := controller.Describe(store.instances[0])
	require.Equal(t, StatusError, status.Status)
	require.Equal(t, ReasonObserverError, status.Reason)
}

func TestRunDegradesWhenStartupResyncFails(t *testing.T) {
	t.Parallel()

	store := newFakeInstanceStore([]Instance{{
		ID:             "inst-err",
		Name:           "inst-err",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.60",
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: "1m"},
	}})
	source := &fakeConnectionSource{
		listErr: errors.New("conntrack permission denied"),
	}
	controller := NewController(store, source, ControllerOptions{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- controller.Run(ctx)
	}()

	select {
	case err := <-done:
		require.NoError(t, err, "controller should wait for cancellation instead of exiting on startup resync failure")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	require.NoError(t, <-done)

	status := controller.Describe(store.instances[0])
	require.Equal(t, StatusError, status.Status)
	require.Equal(t, ReasonObserverError, status.Reason)
}

func TestHandleStandbyTimerCallsStandbyAndClearsState(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	store := newFakeInstanceStore([]Instance{{
		ID:             "inst-standby",
		Name:           "inst-standby",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.61",
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: "1m"},
		Runtime: &Runtime{
			IdleSince: &idleSince,
		},
	}})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return idleSince.Add(time.Minute) },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	controller.handleStandbyTimer(context.Background(), "inst-standby")
	controller.standbyWG.Wait()

	require.Equal(t, []string{"inst-standby"}, store.standbyCalls())
	require.Nil(t, store.persistedRuntime["inst-standby"])

	controller.mu.RLock()
	state := controller.states["inst-standby"]
	require.NotNil(t, state)
	assert.Nil(t, state.compiledPolicy)
	assert.Nil(t, state.activeInbound)
	assert.Nil(t, state.idleSince)
	assert.Nil(t, state.lastInboundAt)
	assert.Nil(t, state.nextStandbyAt)
	assert.False(t, state.standbyRequested)
	controller.mu.RUnlock()
}

func TestHandleStandbyTimerFailureRearmsIdleCountdown(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	now := idleSince.Add(time.Minute)
	store := newFakeInstanceStore([]Instance{{
		ID:             "inst-standby-fail",
		Name:           "inst-standby-fail",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.62",
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: "1m"},
		Runtime: &Runtime{
			IdleSince: &idleSince,
		},
	}})
	store.standbyErr = errors.New("standby failed")
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return now },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	controller.handleStandbyTimer(context.Background(), "inst-standby-fail")
	controller.standbyWG.Wait()

	require.Equal(t, []string{"inst-standby-fail"}, store.standbyCalls())
	require.NotNil(t, store.persistedRuntime["inst-standby-fail"])
	require.NotNil(t, store.persistedRuntime["inst-standby-fail"].IdleSince)
	assert.Equal(t, now, *store.persistedRuntime["inst-standby-fail"].IdleSince)

	controller.mu.RLock()
	state := controller.states["inst-standby-fail"]
	require.NotNil(t, state)
	assert.NotNil(t, state.compiledPolicy)
	assert.False(t, state.standbyRequested)
	assert.NotNil(t, state.idleSince)
	assert.Equal(t, now, *state.idleSince)
	require.NotNil(t, state.nextStandbyAt)
	assert.Equal(t, now.Add(time.Minute), *state.nextStandbyAt)
	controller.mu.RUnlock()
}

func TestHandleStandbyTimerSkipsStandbyWhenConntrackReportsConnection(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	inst := idleTestInstance("inst-missed-event", "192.168.100.63", idleSince)
	store := newFakeInstanceStore([]Instance{inst})
	source := &fakeConnectionSource{}
	controller := NewController(store, source, ControllerOptions{
		Now: func() time.Time { return idleSince.Add(time.Minute) },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	// The NEW event for this connection never reached the controller, so only
	// the conntrack table knows the instance is busy.
	source.connections = []Connection{{
		OriginalSourceIP:        mustAddr("1.2.3.4"),
		OriginalSourcePort:      50100,
		OriginalDestinationIP:   mustAddr("192.168.100.63"),
		OriginalDestinationPort: 8080,
		TCPState:                TCPStateEstablished,
	}}

	controller.handleStandbyTimer(context.Background(), "inst-missed-event")
	controller.standbyWG.Wait()

	assert.Empty(t, store.standbyCalls())
	require.NotNil(t, store.persistedRuntime["inst-missed-event"])
	assert.Nil(t, store.persistedRuntime["inst-missed-event"].IdleSince)

	controller.mu.RLock()
	state := controller.states["inst-missed-event"]
	require.NotNil(t, state)
	assert.Len(t, state.activeInbound, 1)
	assert.Nil(t, state.idleSince)
	assert.Nil(t, state.timer)
	assert.NotNil(t, state.reconcileTimer)
	assert.False(t, state.standbyRequested)
	controller.mu.RUnlock()
}

func TestHandleStandbyTimerRearmsCountdownWhenConfirmationFails(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	now := idleSince.Add(time.Minute)
	inst := idleTestInstance("inst-confirm-fail", "192.168.100.64", idleSince)
	store := newFakeInstanceStore([]Instance{inst})
	source := &fakeConnectionSource{}
	controller := NewController(store, source, ControllerOptions{
		Now: func() time.Time { return now },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	source.listErr = errors.New("conntrack dump failed")

	controller.handleStandbyTimer(context.Background(), "inst-confirm-fail")
	controller.standbyWG.Wait()

	assert.Empty(t, store.standbyCalls())
	require.NotNil(t, store.persistedRuntime["inst-confirm-fail"])
	require.NotNil(t, store.persistedRuntime["inst-confirm-fail"].IdleSince)
	assert.Equal(t, now, *store.persistedRuntime["inst-confirm-fail"].IdleSince)

	controller.mu.RLock()
	state := controller.states["inst-confirm-fail"]
	require.NotNil(t, state)
	assert.False(t, state.standbyRequested)
	require.NotNil(t, state.idleSince)
	assert.Equal(t, now, *state.idleSince)
	require.NotNil(t, state.nextStandbyAt)
	assert.Equal(t, now.Add(time.Minute), *state.nextStandbyAt)
	controller.mu.RUnlock()
}

func TestHandleStandbyTimerKeepsActiveStateWhenConfirmationFails(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	now := idleSince.Add(time.Minute)
	inst := idleTestInstance("inst-confirm-active", "192.168.100.67", idleSince)
	store := newFakeInstanceStore([]Instance{inst})
	source := &fakeConnectionSource{}
	controller := NewController(store, source, ControllerOptions{
		Now: func() time.Time { return now },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	// Inbound activity arrives after the standby timer was already queued.
	controller.handleConnectionEvent(context.Background(), ConnectionEvent{
		Type: ConnectionEventNew,
		Connection: Connection{
			OriginalSourceIP:        mustAddr("1.2.3.4"),
			OriginalSourcePort:      50300,
			OriginalDestinationIP:   mustAddr("192.168.100.67"),
			OriginalDestinationPort: 8080,
			TCPState:                TCPStateEstablished,
		},
		ObservedAt: now,
	})
	source.listErr = errors.New("conntrack dump failed")

	controller.handleStandbyTimer(context.Background(), "inst-confirm-active")
	controller.standbyWG.Wait()

	assert.Empty(t, store.standbyCalls())
	require.NotNil(t, store.persistedRuntime["inst-confirm-active"])
	assert.Nil(t, store.persistedRuntime["inst-confirm-active"].IdleSince)

	controller.mu.RLock()
	state := controller.states["inst-confirm-active"]
	require.NotNil(t, state)
	assert.Len(t, state.activeInbound, 1)
	assert.Nil(t, state.idleSince)
	assert.Nil(t, state.timer)
	controller.mu.RUnlock()
}

func TestHandleStandbyTimerAbortsWhenTimerRearmedDuringConfirmation(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	now := idleSince.Add(time.Minute)
	inst := idleTestInstance("inst-confirm-rearm", "192.168.100.68", idleSince)
	store := newFakeInstanceStore([]Instance{inst})
	source := &hookedConnectionSource{}
	controller := NewController(store, source, ControllerOptions{
		Now: func() time.Time { return now },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	// A standby worker failing mid-confirmation restarts the countdown, the same
	// way executeStandby's failure path does.
	var rearmed *time.Timer
	source.hook = func() {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		state := controller.states["inst-confirm-rearm"]
		state.idleSince = &now
		controller.armTimerLocked("inst-confirm-rearm", state, now)
		rearmed = state.timer
	}

	controller.handleStandbyTimer(context.Background(), "inst-confirm-rearm")
	controller.standbyWG.Wait()

	assert.Empty(t, store.standbyCalls())
	require.NotNil(t, rearmed)
	assert.False(t, rearmed.Stop(), "the timer armed during confirmation should have been replaced, not left running")

	status := controller.Describe(inst)
	require.NotNil(t, status.NextStandbyAt)
	assert.Equal(t, now.Add(time.Minute), *status.NextStandbyAt)
}

func TestHandleStandbyTimerAbortsWhenHoldPlacedDuringConfirmation(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	now := idleSince.Add(time.Minute)
	inst := idleTestInstance("inst-hold-race", "192.168.100.69", idleSince)
	store := newFakeInstanceStore([]Instance{inst})
	source := &hookedConnectionSource{}
	controller := NewController(store, source, ControllerOptions{
		Now: func() time.Time { return now },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	// The hold lands while the confirmation dump is in flight, after the timer
	// delivery already passed the deadline check.
	source.hook = func() {
		_, err := controller.HoldStandby(context.Background(), inst)
		require.NoError(t, err)
	}

	controller.handleStandbyTimer(context.Background(), "inst-hold-race")
	controller.standbyWG.Wait()

	assert.Empty(t, store.standbyCalls())

	status := controller.Describe(inst)
	require.NotNil(t, status.HoldUntil)
	assert.Equal(t, now.Add(time.Minute), *status.HoldUntil)
	require.NotNil(t, status.NextStandbyAt)
	assert.Equal(t, now.Add(time.Minute), *status.NextStandbyAt)
}

func TestStreamRestoreResyncsFromConntrackTable(t *testing.T) {
	t.Parallel()

	inst := Instance{
		ID:             "inst-restore",
		Name:           "inst-restore",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.65",
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: "1m"},
	}
	store := newFakeInstanceStore([]Instance{inst})
	source := &restoreConnectionSource{streams: make(chan *fakeConnectionStream, 4)}
	controller := NewController(store, source, ControllerOptions{
		ReconnectDelay: time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- controller.Run(ctx) }()

	stream := <-source.streams
	require.Equal(t, StatusIdleCountdown, controller.Describe(inst).Status)

	// The connection opens while the subscription is down, so its NEW event is
	// never delivered.
	source.setConnections([]Connection{{
		OriginalSourceIP:        mustAddr("1.2.3.4"),
		OriginalSourcePort:      50200,
		OriginalDestinationIP:   mustAddr("192.168.100.65"),
		OriginalDestinationPort: 8080,
		TCPState:                TCPStateEstablished,
	}})
	stream.errs <- errors.New("recv conntrack events: no buffer space available")

	require.Eventually(t, func() bool {
		return controller.Describe(inst).Status == StatusActive
	}, time.Second, 5*time.Millisecond)

	cancel()
	require.NoError(t, <-done)
}

func TestStreamRestoreKeepsObserverConnectedWhenResyncFails(t *testing.T) {
	t.Parallel()

	inst := Instance{
		ID:             "inst-restore-degraded",
		Name:           "inst-restore-degraded",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.66",
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: "1m"},
	}
	store := newFakeInstanceStore([]Instance{inst})
	source := &restoreConnectionSource{streams: make(chan *fakeConnectionStream, 4)}
	controller := NewController(store, source, ControllerOptions{
		ReconnectDelay: time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- controller.Run(ctx) }()

	stream := <-source.streams
	listsBeforeRestore := source.listCalls()

	source.setListErr(errors.New("conntrack dump failed"))
	stream.errs <- errors.New("recv conntrack events: no buffer space available")

	// The reseed after the restore fails, but the subscription itself is live,
	// so the instance must not be left reporting an observer error.
	require.Eventually(t, func() bool {
		return source.listCalls() > listsBeforeRestore && controller.Describe(inst).Status != StatusError
	}, time.Second, 5*time.Millisecond)

	cancel()
	require.NoError(t, <-done)
}

func mustAddr(raw string) netip.Addr {
	return netip.MustParseAddr(raw)
}

// hookedConnectionSource runs a callback inside ListConnections so a test can
// mutate controller state while a conntrack dump is in flight.
type hookedConnectionSource struct {
	hook func()
}

func (s *hookedConnectionSource) ListConnections(context.Context) ([]Connection, error) {
	if s.hook != nil {
		s.hook()
	}
	return nil, nil
}

func (s *hookedConnectionSource) OpenStream(context.Context) (ConnectionStream, error) {
	return &fakeConnectionStream{
		events: make(chan ConnectionEvent),
		errs:   make(chan error),
	}, nil
}

// restoreConnectionSource hands out the streams it creates so a test can fail
// one, and serializes connection access for controllers running in Run mode.
type restoreConnectionSource struct {
	mu          sync.Mutex
	connections []Connection
	listErr     error
	lists       int
	streams     chan *fakeConnectionStream
}

func (s *restoreConnectionSource) ListConnections(context.Context) ([]Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return append([]Connection(nil), s.connections...), nil
}

func (s *restoreConnectionSource) setConnections(conns []Connection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connections = conns
}

func (s *restoreConnectionSource) setListErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listErr = err
}

func (s *restoreConnectionSource) listCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists
}

func (s *restoreConnectionSource) OpenStream(context.Context) (ConnectionStream, error) {
	stream := &fakeConnectionStream{
		events: make(chan ConnectionEvent),
		errs:   make(chan error, 1),
	}
	s.streams <- stream
	return stream, nil
}

func idleTestInstance(id, ip string, idleSince time.Time) Instance {
	since := idleSince
	return Instance{
		ID:             id,
		Name:           id,
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             ip,
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: "1m"},
		Runtime:        &Runtime{IdleSince: &since},
	}
}

func TestStandbyExecutionBoundedByMaxConcurrent(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-a", "192.168.100.70", idleSince),
		idleTestInstance("inst-b", "192.168.100.71", idleSince),
		idleTestInstance("inst-c", "192.168.100.72", idleSince),
		idleTestInstance("inst-d", "192.168.100.73", idleSince),
	})
	store.standbyStarted = make(chan string, 4)
	store.standbyRelease = make(chan struct{})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now:                   func() time.Time { return idleSince.Add(time.Minute) },
		MaxConcurrentStandbys: 2,
	})

	require.NoError(t, controller.startupResync(context.Background()))

	for _, id := range []string{"inst-a", "inst-b", "inst-c", "inst-d"} {
		controller.handleStandbyTimer(context.Background(), id)
	}

	started := map[string]struct{}{}
	for len(started) < 2 {
		started[<-store.standbyStarted] = struct{}{}
	}
	select {
	case id := <-store.standbyStarted:
		t.Fatalf("standby for %s started beyond the concurrency cap", id)
	case <-time.After(100 * time.Millisecond):
	}

	close(store.standbyRelease)
	controller.standbyWG.Wait()
	assert.Len(t, store.standbyCalls(), 4)
}

func TestStandbyCancelledByInboundActivityWhileQueued(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-first", "192.168.100.80", idleSince),
		idleTestInstance("inst-queued", "192.168.100.81", idleSince),
	})
	store.standbyStarted = make(chan string, 2)
	store.standbyRelease = make(chan struct{})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now:                   func() time.Time { return idleSince.Add(time.Minute) },
		MaxConcurrentStandbys: 1,
	})

	require.NoError(t, controller.startupResync(context.Background()))

	controller.handleStandbyTimer(context.Background(), "inst-first")
	require.Equal(t, "inst-first", <-store.standbyStarted)
	controller.handleStandbyTimer(context.Background(), "inst-queued")

	controller.handleConnectionEvent(context.Background(), ConnectionEvent{
		Type: ConnectionEventNew,
		Connection: Connection{
			OriginalSourceIP:        mustAddr("1.2.3.4"),
			OriginalSourcePort:      50002,
			OriginalDestinationIP:   mustAddr("192.168.100.81"),
			OriginalDestinationPort: 8080,
			TCPState:                TCPStateEstablished,
		},
		ObservedAt: idleSince.Add(time.Minute),
	})

	close(store.standbyRelease)
	controller.standbyWG.Wait()
	assert.Equal(t, []string{"inst-first"}, store.standbyCalls())
}

func TestStandbyDispatchDeduplicatesPendingRequests(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-dup", "192.168.100.90", idleSince),
	})
	store.standbyStarted = make(chan string, 2)
	store.standbyRelease = make(chan struct{})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return idleSince.Add(time.Minute) },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	controller.handleStandbyTimer(context.Background(), "inst-dup")
	require.Equal(t, "inst-dup", <-store.standbyStarted)
	controller.handleStandbyTimer(context.Background(), "inst-dup")

	close(store.standbyRelease)
	controller.standbyWG.Wait()
	assert.Equal(t, []string{"inst-dup"}, store.standbyCalls())
}

func TestStandbyNotFoundDropsStateWithoutRearming(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-gone", "192.168.100.91", idleSince),
	})
	store.standbyErr = fmt.Errorf("%w: deleted concurrently", ErrInstanceNotFound)
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return idleSince.Add(time.Minute) },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	controller.handleStandbyTimer(context.Background(), "inst-gone")
	controller.standbyWG.Wait()

	require.Equal(t, []string{"inst-gone"}, store.standbyCalls())
	controller.mu.RLock()
	_, tracked := controller.states["inst-gone"]
	controller.mu.RUnlock()
	assert.False(t, tracked)
}

func TestResyncDuringExecutingStandbyDoesNotDoubleDispatch(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	inst := idleTestInstance("inst-racing", "192.168.100.92", idleSince)
	store := newFakeInstanceStore([]Instance{inst})
	store.standbyStarted = make(chan string, 2)
	store.standbyRelease = make(chan struct{})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return idleSince.Add(time.Minute) },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	controller.handleStandbyTimer(context.Background(), "inst-racing")
	require.Equal(t, "inst-racing", <-store.standbyStarted)

	// A lifecycle refresh while the standby executes resets standbyRequested
	// and re-arms an already-elapsed idle timer.
	refreshed := cloneInstance(inst)
	require.NoError(t, controller.handleInstanceEvent(context.Background(), InstanceEvent{
		Action:     InstanceEventUpdate,
		InstanceID: "inst-racing",
		Instance:   &refreshed,
	}))
	controller.handleStandbyTimer(context.Background(), "inst-racing")

	select {
	case <-store.standbyStarted:
		t.Fatal("second standby dispatched while one was executing")
	case <-time.After(100 * time.Millisecond):
	}

	close(store.standbyRelease)
	controller.standbyWG.Wait()
	assert.Equal(t, []string{"inst-racing"}, store.standbyCalls())
}

func TestActiveReconcileClearsPendingStandbyRequest(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-busy", "192.168.100.93", idleSince),
		idleTestInstance("inst-pending", "192.168.100.94", idleSince),
	})
	store.standbyStarted = make(chan string, 2)
	store.standbyRelease = make(chan struct{})
	source := &fakeConnectionSource{}
	controller := NewController(store, source, ControllerOptions{
		Now:                   func() time.Time { return idleSince.Add(time.Minute) },
		MaxConcurrentStandbys: 1,
	})

	require.NoError(t, controller.startupResync(context.Background()))

	controller.handleStandbyTimer(context.Background(), "inst-busy")
	require.Equal(t, "inst-busy", <-store.standbyStarted)
	controller.handleStandbyTimer(context.Background(), "inst-pending")

	// Reconcile discovers an active inbound connection for the queued instance.
	source.connections = []Connection{{
		OriginalSourceIP:        mustAddr("1.2.3.4"),
		OriginalSourcePort:      50003,
		OriginalDestinationIP:   mustAddr("192.168.100.94"),
		OriginalDestinationPort: 8080,
		TCPState:                TCPStateEstablished,
	}}
	controller.handleActiveReconcile(context.Background(), "inst-pending")

	controller.mu.RLock()
	pending := controller.states["inst-pending"]
	require.NotNil(t, pending)
	assert.False(t, pending.standbyRequested)
	assert.Len(t, pending.activeInbound, 1)
	controller.mu.RUnlock()

	close(store.standbyRelease)
	controller.standbyWG.Wait()
	assert.Equal(t, []string{"inst-busy"}, store.standbyCalls())
}

func TestStandbyFailureWithMidFlightActivityDoesNotRearmIdle(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-fail-busy", "192.168.100.95", idleSince),
	})
	store.standbyErr = errors.New("standby failed")
	store.standbyStarted = make(chan string, 1)
	store.standbyRelease = make(chan struct{})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return idleSince.Add(time.Minute) },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	controller.handleStandbyTimer(context.Background(), "inst-fail-busy")
	require.Equal(t, "inst-fail-busy", <-store.standbyStarted)

	// Inbound activity lands while the standby attempt is executing.
	controller.handleConnectionEvent(context.Background(), ConnectionEvent{
		Type: ConnectionEventNew,
		Connection: Connection{
			OriginalSourceIP:        mustAddr("1.2.3.4"),
			OriginalSourcePort:      50004,
			OriginalDestinationIP:   mustAddr("192.168.100.95"),
			OriginalDestinationPort: 8080,
			TCPState:                TCPStateEstablished,
		},
		ObservedAt: idleSince.Add(time.Minute),
	})

	close(store.standbyRelease)
	controller.standbyWG.Wait()

	controller.mu.RLock()
	state := controller.states["inst-fail-busy"]
	require.NotNil(t, state)
	assert.Len(t, state.activeInbound, 1)
	assert.False(t, state.standbyRequested)
	assert.Nil(t, state.idleSince)
	assert.Nil(t, state.nextStandbyAt)
	controller.mu.RUnlock()

	store.mu.Lock()
	persisted := cloneRuntime(store.persistedRuntime["inst-fail-busy"])
	store.mu.Unlock()
	require.NotNil(t, persisted)
	assert.Nil(t, persisted.IdleSince, "failure must not persist a false idle window while connections are active")
}

func TestShutdownCancelsQueuedStandbyAndDrainsWorkers(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-active-slot", "192.168.100.96", idleSince),
		idleTestInstance("inst-waiting", "192.168.100.97", idleSince),
	})
	store.standbyStarted = make(chan string, 2)
	store.standbyRelease = make(chan struct{})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now:                   func() time.Time { return idleSince.Add(time.Minute) },
		MaxConcurrentStandbys: 1,
	})

	require.NoError(t, controller.startupResync(context.Background()))

	ctx, cancel := context.WithCancel(context.Background())
	controller.handleStandbyTimer(ctx, "inst-active-slot")
	require.Equal(t, "inst-active-slot", <-store.standbyStarted)
	controller.handleStandbyTimer(ctx, "inst-waiting")

	// Shutdown while one worker holds the slot and the other waits on it.
	cancel()
	close(store.standbyRelease)
	controller.standbyWG.Wait()

	assert.Equal(t, []string{"inst-active-slot"}, store.standbyCalls(),
		"queued standby must be abandoned on shutdown, in-flight one must drain")
}

func TestErroringRefreshDoesNotCancelQueuedStandby(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-holder", "192.168.100.98", idleSince),
		idleTestInstance("inst-refresh-err", "192.168.100.99", idleSince),
	})
	store.standbyStarted = make(chan string, 2)
	store.standbyRelease = make(chan struct{})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now:                   func() time.Time { return idleSince.Add(time.Minute) },
		MaxConcurrentStandbys: 1,
	})

	require.NoError(t, controller.startupResync(context.Background()))

	controller.handleStandbyTimer(context.Background(), "inst-holder")
	require.Equal(t, "inst-holder", <-store.standbyStarted)
	controller.handleStandbyTimer(context.Background(), "inst-refresh-err")

	// A refresh that errors after the queued dispatch (invalid instance IP
	// fails connection matching) must leave the queued attempt intact.
	broken := idleTestInstance("inst-refresh-err", "not-an-ip", idleSince)
	require.Error(t, controller.handleInstanceEvent(context.Background(), InstanceEvent{
		Action:     InstanceEventUpdate,
		InstanceID: "inst-refresh-err",
		Instance:   &broken,
	}))

	controller.mu.RLock()
	require.NotNil(t, controller.states["inst-refresh-err"])
	assert.True(t, controller.states["inst-refresh-err"].standbyRequested)
	controller.mu.RUnlock()

	close(store.standbyRelease)
	controller.standbyWG.Wait()
	assert.ElementsMatch(t, []string{"inst-holder", "inst-refresh-err"}, store.standbyCalls())
}

func TestRefreshPersistFailureStillArmsIdleTimer(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	inst := Instance{
		ID:             "inst-persist-err",
		Name:           "inst-persist-err",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.100",
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: "1m"},
	}
	store := newFakeInstanceStore([]Instance{inst})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return idleSince },
	})
	store.setRuntimeErr = errors.New("metadata write failed")

	// Fresh idle state forces the persist that fails; the countdown must be
	// armed regardless.
	refreshed := cloneInstance(inst)
	require.NoError(t, controller.handleInstanceEvent(context.Background(), InstanceEvent{
		Action:     InstanceEventCreate,
		InstanceID: "inst-persist-err",
		Instance:   &refreshed,
	}))

	controller.mu.RLock()
	state := controller.states["inst-persist-err"]
	require.NotNil(t, state)
	assert.NotNil(t, state.nextStandbyAt)
	assert.NotNil(t, state.idleSince)
	controller.mu.RUnlock()
}

func TestHoldStandbyExtendsArmedCountdown(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	now := idleSince.Add(50 * time.Second)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-hold", "192.168.100.100", idleSince),
	})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return now },
	})

	require.NoError(t, controller.startupResync(context.Background()))
	persistedBefore := cloneRuntime(store.persistedRuntime["inst-hold"])

	snapshot, err := controller.HoldStandby(context.Background(), store.instances[0])
	require.NoError(t, err)
	assert.Equal(t, StatusIdleCountdown, snapshot.Status)
	require.NotNil(t, snapshot.HoldUntil)
	assert.Equal(t, now.Add(time.Minute), *snapshot.HoldUntil)
	require.NotNil(t, snapshot.NextStandbyAt)
	assert.Equal(t, now.Add(time.Minute), *snapshot.NextStandbyAt)

	// Holds are in-memory only: the persisted countdown is untouched.
	assert.Equal(t, persistedBefore, store.persistedRuntime["inst-hold"])

	// A resync must keep the held deadline.
	require.NoError(t, controller.startupResync(context.Background()))
	controller.mu.RLock()
	state := controller.states["inst-hold"]
	require.NotNil(t, state)
	require.NotNil(t, state.nextStandbyAt)
	assert.Equal(t, now.Add(time.Minute), *state.nextStandbyAt)
	controller.mu.RUnlock()
}

func TestHoldStandbyCancelsQueuedStandby(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	now := idleSince.Add(time.Minute)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-first", "192.168.100.101", idleSince),
		idleTestInstance("inst-queued", "192.168.100.102", idleSince),
	})
	store.standbyStarted = make(chan string, 2)
	store.standbyRelease = make(chan struct{})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now:                   func() time.Time { return now },
		MaxConcurrentStandbys: 1,
	})

	require.NoError(t, controller.startupResync(context.Background()))

	controller.handleStandbyTimer(context.Background(), "inst-first")
	require.Equal(t, "inst-first", <-store.standbyStarted)
	controller.handleStandbyTimer(context.Background(), "inst-queued")

	snapshot, err := controller.HoldStandby(context.Background(), store.instances[1])
	require.NoError(t, err)
	require.NotNil(t, snapshot.NextStandbyAt)
	assert.Equal(t, now.Add(time.Minute), *snapshot.NextStandbyAt)

	close(store.standbyRelease)
	controller.standbyWG.Wait()
	assert.Equal(t, []string{"inst-first"}, store.standbyCalls())
}

func TestHoldStandbyFailsWhileStandbyExecuting(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-executing", "192.168.100.103", idleSince),
	})
	store.standbyStarted = make(chan string, 1)
	store.standbyRelease = make(chan struct{})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return idleSince.Add(time.Minute) },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	controller.handleStandbyTimer(context.Background(), "inst-executing")
	require.Equal(t, "inst-executing", <-store.standbyStarted)

	_, err := controller.HoldStandby(context.Background(), store.instances[0])
	assert.ErrorIs(t, err, ErrStandbyInProgress)

	close(store.standbyRelease)
	controller.standbyWG.Wait()
	assert.Equal(t, []string{"inst-executing"}, store.standbyCalls())
}

func TestHoldStandbyIgnoresStaleTimerDelivery(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	now := idleSince.Add(time.Minute)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-stale", "192.168.100.104", idleSince),
	})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return now },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	// The countdown expires and enqueues a timer delivery; before the run
	// loop processes it, a hold moves the deadline out.
	snapshot, err := controller.HoldStandby(context.Background(), store.instances[0])
	require.NoError(t, err)
	require.NotNil(t, snapshot.NextStandbyAt)
	require.Equal(t, now.Add(time.Minute), *snapshot.NextStandbyAt)

	controller.handleStandbyTimer(context.Background(), "inst-stale")
	controller.standbyWG.Wait()

	assert.Empty(t, store.standbyCalls())
	controller.mu.RLock()
	state := controller.states["inst-stale"]
	require.NotNil(t, state)
	assert.False(t, state.standbyRequested)
	require.NotNil(t, state.nextStandbyAt)
	assert.Equal(t, now.Add(time.Minute), *state.nextStandbyAt)
	controller.mu.RUnlock()
}

func TestHoldStandbySurvivesConnectionChurn(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 6, 11, 0, 0, 0, time.UTC)
	store := newFakeInstanceStore([]Instance{{
		ID:             "inst-churn",
		Name:           "inst-churn",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.105",
		AutoStandby:    &Policy{Enabled: true, IdleTimeout: "1m"},
	}})
	conn := Connection{
		OriginalSourceIP:        mustAddr("1.2.3.4"),
		OriginalSourcePort:      50010,
		OriginalDestinationIP:   mustAddr("192.168.100.105"),
		OriginalDestinationPort: 8080,
		TCPState:                TCPStateEstablished,
	}
	source := &fakeConnectionSource{connections: []Connection{conn}}
	controller := NewController(store, source, ControllerOptions{
		Now: func() time.Time { return now },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	snapshot, err := controller.HoldStandby(context.Background(), store.instances[0])
	require.NoError(t, err)
	assert.Equal(t, StatusActive, snapshot.Status)
	require.NotNil(t, snapshot.HoldUntil)
	assert.Equal(t, now.Add(time.Minute), *snapshot.HoldUntil)

	// The held connection closing 50s later must arm the timer no earlier
	// than hold_until.
	controller.handleConnectionEvent(context.Background(), ConnectionEvent{
		Type:       ConnectionEventDestroy,
		Connection: conn,
		ObservedAt: now.Add(50 * time.Second),
	})

	controller.mu.RLock()
	state := controller.states["inst-churn"]
	require.NotNil(t, state)
	require.NotNil(t, state.nextStandbyAt)
	assert.Equal(t, now.Add(50*time.Second).Add(time.Minute), *state.nextStandbyAt)
	controller.mu.RUnlock()
}

func TestHoldStandbyOnUntrackedEligibleInstance(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 6, 11, 30, 0, 0, time.UTC)
	idleSince := now.Add(-time.Minute)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-unseeded", "192.168.100.106", idleSince),
	})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return now },
	})

	// No startup resync: the hold races controller startup.
	snapshot, err := controller.HoldStandby(context.Background(), store.instances[0])
	require.NoError(t, err)
	require.NotNil(t, snapshot.HoldUntil)
	assert.Equal(t, now.Add(time.Minute), *snapshot.HoldUntil)

	// The seeding resync finds an already expired persisted idle_since; the
	// hold must keep the timer from arming any earlier than hold_until.
	require.NoError(t, controller.startupResync(context.Background()))
	controller.mu.RLock()
	state := controller.states["inst-unseeded"]
	require.NotNil(t, state)
	require.NotNil(t, state.compiledPolicy)
	require.NotNil(t, state.nextStandbyAt)
	assert.Equal(t, now.Add(time.Minute), *state.nextStandbyAt)
	controller.mu.RUnlock()
}

func TestHoldStandbyNoopWhenPolicyDisabled(t *testing.T) {
	t.Parallel()

	store := newFakeInstanceStore([]Instance{{
		ID:             "inst-disabled",
		Name:           "inst-disabled",
		State:          StateRunning,
		NetworkEnabled: true,
		IP:             "192.168.100.107",
		AutoStandby:    &Policy{Enabled: false, IdleTimeout: "1m"},
	}})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{})

	require.NoError(t, controller.startupResync(context.Background()))

	snapshot, err := controller.HoldStandby(context.Background(), store.instances[0])
	require.NoError(t, err)
	assert.Equal(t, StatusDisabled, snapshot.Status)
	assert.Nil(t, snapshot.HoldUntil)
}

func TestHandleStandbyTimerRearmsWhenDeliveryTooEarly(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	now := idleSince.Add(time.Minute)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-early", "192.168.100.108", idleSince),
	})
	// A backward clock step can make a monotonic timer fire before its wall
	// clock deadline; the delivery must be dropped and the timer replaced.
	stepped := now.Add(-30 * time.Second)
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return now },
	})

	require.NoError(t, controller.startupResync(context.Background()))
	controller.now = func() time.Time { return stepped }

	controller.handleStandbyTimer(context.Background(), "inst-early")
	controller.standbyWG.Wait()

	assert.Empty(t, store.standbyCalls())
	controller.mu.RLock()
	state := controller.states["inst-early"]
	require.NotNil(t, state)
	require.NotNil(t, state.timer)
	require.NotNil(t, state.nextStandbyAt)
	assert.Equal(t, now, *state.nextStandbyAt)
	controller.mu.RUnlock()
}

func TestDescribeOmitsExpiredHold(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 6, 11, 0, 0, 0, time.UTC)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-expired-hold", "192.168.100.109", now),
	})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return now },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	snapshot, err := controller.HoldStandby(context.Background(), store.instances[0])
	require.NoError(t, err)
	require.NotNil(t, snapshot.HoldUntil)

	controller.now = func() time.Time { return now.Add(2 * time.Minute) }
	assert.Nil(t, controller.Describe(store.instances[0]).HoldUntil)
}
