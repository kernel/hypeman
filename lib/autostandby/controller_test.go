package autostandby

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeInstanceStore struct {
	instances        []Instance
	standbyIDs       []string
	persistedRuntime map[string]*Runtime
	events           chan InstanceEvent
	standbyErr       error
	listErr          error
}

func newFakeInstanceStore(instances []Instance) *fakeInstanceStore {
	return &fakeInstanceStore{
		instances:        append([]Instance(nil), instances...),
		persistedRuntime: make(map[string]*Runtime),
		events:           make(chan InstanceEvent, 16),
	}
}

func (f *fakeInstanceStore) ListInstances(context.Context) ([]Instance, error) {
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
	f.standbyIDs = append(f.standbyIDs, id)
	return f.standbyErr
}

func (f *fakeInstanceStore) SetRuntime(_ context.Context, id string, runtime *Runtime) error {
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

	require.Equal(t, []string{"inst-standby"}, store.standbyIDs)
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

	require.Equal(t, []string{"inst-standby-fail"}, store.standbyIDs)
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

func mustAddr(raw string) netip.Addr {
	return netip.MustParseAddr(raw)
}
