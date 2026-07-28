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
	mu          sync.Mutex
	connections []Connection
	listErr     error
	openErr     error
	onList      func()
}

func (f *fakeConnectionSource) ListConnections(context.Context) ([]Connection, error) {
	f.mu.Lock()
	onList := f.onList
	f.mu.Unlock()
	// Runs while the caller holds no lock, so a test can land a conntrack event
	// mid-read the way the controller's event loop would.
	if onList != nil {
		onList()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]Connection(nil), f.connections...), nil
}

func (f *fakeConnectionSource) setOnList(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onList = fn
}

func (f *fakeConnectionSource) setConnections(conns ...Connection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connections = append([]Connection(nil), conns...)
}

func (f *fakeConnectionSource) setListErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listErr = err
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

func mustAddr(raw string) netip.Addr {
	return netip.MustParseAddr(raw)
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

// The tracked connection view is event-driven, and the reconcile that re-reads
// the host table is cancelled once it drains, so a missed or misread conntrack
// event leaves it empty while the client is still attached. Standby has to
// confirm against the host table rather than trust that view.
func TestStandbySkippedWhenConntrackStillShowsLiveInboundConnection(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-stale", "192.168.100.120", idleSince),
	})
	source := &fakeConnectionSource{}
	controller := NewController(store, source, ControllerOptions{
		Now: func() time.Time { return idleSince.Add(time.Minute) },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	controller.mu.RLock()
	require.Empty(t, controller.states["inst-stale"].activeInbound)
	controller.mu.RUnlock()

	source.setConnections(Connection{
		OriginalSourceIP:        mustAddr("1.2.3.4"),
		OriginalSourcePort:      50010,
		OriginalDestinationIP:   mustAddr("192.168.100.120"),
		OriginalDestinationPort: 8080,
		TCPState:                TCPStateEstablished,
	})

	controller.handleStandbyTimer(context.Background(), "inst-stale")
	controller.standbyWG.Wait()

	assert.Empty(t, store.standbyCalls(), "a guest with a live inbound flow must not be suspended")
	controller.mu.RLock()
	defer controller.mu.RUnlock()
	state := controller.states["inst-stale"]
	require.NotNil(t, state)
	assert.Len(t, state.activeInbound, 1)
	assert.False(t, state.standbyRequested)
	assert.Nil(t, state.idleSince)
}

func TestStandbyDeferredWhenConnectionListUnavailable(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-unreadable", "192.168.100.121", idleSince),
	})
	source := &fakeConnectionSource{}
	controller := NewController(store, source, ControllerOptions{
		Now: func() time.Time { return idleSince.Add(time.Minute) },
	})

	require.NoError(t, controller.startupResync(context.Background()))
	source.setListErr(errors.New("conntrack permission denied"))

	controller.handleStandbyTimer(context.Background(), "inst-unreadable")
	controller.standbyWG.Wait()

	assert.Empty(t, store.standbyCalls(), "suspending blind is what strands connections")
	controller.mu.RLock()
	defer controller.mu.RUnlock()
	state := controller.states["inst-unreadable"]
	require.NotNil(t, state)
	assert.False(t, state.standbyRequested)
	// Re-armed rather than dropped, so the next cycle retries.
	assert.NotNil(t, state.idleSince)
	assert.NotNil(t, state.timer)
}

func TestStandbyProceedsWhenHostTableAgreesInstanceIsIdle(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-quiet", "192.168.100.122", idleSince),
	})
	controller := NewController(store, &fakeConnectionSource{}, ControllerOptions{
		Now: func() time.Time { return idleSince.Add(time.Minute) },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	controller.handleStandbyTimer(context.Background(), "inst-quiet")
	controller.standbyWG.Wait()

	assert.Equal(t, []string{"inst-quiet"}, store.standbyCalls())
}

// The pre-suspend read happens without the controller lock, so a conntrack event
// can register a connection while it is in flight. Deferring must not then
// overwrite that fresh activity with a new idle countdown.
func TestStandbyDeferralKeepsActivityThatLandedDuringTheRead(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 6, 10, 55, 0, 0, time.UTC)
	store := newFakeInstanceStore([]Instance{
		idleTestInstance("inst-raced-read", "192.168.100.123", idleSince),
	})
	source := &fakeConnectionSource{}
	controller := NewController(store, source, ControllerOptions{
		Now: func() time.Time { return idleSince.Add(time.Minute) },
	})

	require.NoError(t, controller.startupResync(context.Background()))

	source.setListErr(errors.New("conntrack permission denied"))
	source.setOnList(func() {
		controller.handleConnectionEvent(context.Background(), ConnectionEvent{
			Type: ConnectionEventNew,
			Connection: Connection{
				OriginalSourceIP:        mustAddr("1.2.3.4"),
				OriginalSourcePort:      50013,
				OriginalDestinationIP:   mustAddr("192.168.100.123"),
				OriginalDestinationPort: 8080,
				TCPState:                TCPStateEstablished,
			},
			ObservedAt: idleSince.Add(time.Minute),
		})
	})

	controller.handleStandbyTimer(context.Background(), "inst-raced-read")
	controller.standbyWG.Wait()

	assert.Empty(t, store.standbyCalls())
	controller.mu.RLock()
	defer controller.mu.RUnlock()
	state := controller.states["inst-raced-read"]
	require.NotNil(t, state)
	assert.Len(t, state.activeInbound, 1, "activity that landed during the read must survive")
	assert.Nil(t, state.idleSince, "deferring must not restart the countdown against a live connection")
	assert.Nil(t, state.timer)
	assert.False(t, state.standbyRequested)
}
