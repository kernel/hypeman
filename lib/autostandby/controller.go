package autostandby

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const (
	trackingModeConntrackEventsV4TCP = "conntrack_events_v4_tcp"
	defaultReconnectDelay            = 2 * time.Second
	defaultReconcileDelay            = 2 * time.Second
	defaultSnapshotSyncInterval      = 5 * time.Minute
	defaultMaxConcurrentStandbys     = 16
)

// ErrInstanceNotFound signals that a standby target no longer exists. The
// controller drops its tracking state instead of re-arming the idle timer.
var ErrInstanceNotFound = errors.New("instance not found")

// InstanceEventAction identifies an instance lifecycle change relevant to auto-standby.
type InstanceEventAction string

const (
	InstanceEventCreate  InstanceEventAction = "create"
	InstanceEventUpdate  InstanceEventAction = "update"
	InstanceEventStart   InstanceEventAction = "start"
	InstanceEventStop    InstanceEventAction = "stop"
	InstanceEventStandby InstanceEventAction = "standby"
	InstanceEventRestore InstanceEventAction = "restore"
	InstanceEventDelete  InstanceEventAction = "delete"
	InstanceEventFork    InstanceEventAction = "fork"
)

// InstanceEvent carries an instance lifecycle update into the controller.
type InstanceEvent struct {
	Action     InstanceEventAction
	InstanceID string
	Instance   *Instance
}

// InstanceStore supplies the controller with instance state, lifecycle events,
// runtime persistence, and standby actions.
type InstanceStore interface {
	ListInstances(ctx context.Context) ([]Instance, error)
	StandbyInstance(ctx context.Context, id string) error
	// RestoreInstance brings a standby instance back to running. The
	// controller calls it when inbound activity turns up on an instance it
	// just suspended; wake-on-traffic does not exist, so without this the
	// connection hangs until the client gives up.
	RestoreInstance(ctx context.Context, id string) error
	SetRuntime(ctx context.Context, id string, runtime *Runtime) error
	SubscribeInstanceEvents() (<-chan InstanceEvent, func(), error)
}

type ConnectionEventType string

const (
	ConnectionEventNew     ConnectionEventType = "new"
	ConnectionEventDestroy ConnectionEventType = "destroy"
)

// ConnectionEvent is a single conntrack event delivered from the host observer.
type ConnectionEvent struct {
	Type       ConnectionEventType
	Connection Connection
	ObservedAt time.Time
}

// ConnectionStream is a live conntrack event stream.
type ConnectionStream interface {
	Events() <-chan ConnectionEvent
	Errors() <-chan error
	Close() error
}

// ConnectionSource provides startup snapshots and live conntrack events.
type ConnectionSource interface {
	ListConnections(ctx context.Context) ([]Connection, error)
	OpenStream(ctx context.Context) (ConnectionStream, error)
}

// ControllerOptions configures logging, timing, and observability.
type ControllerOptions struct {
	Log                  *slog.Logger
	Meter                metric.Meter
	Tracer               trace.Tracer
	Now                  func() time.Time
	ReconnectDelay       time.Duration
	ReconcileDelay       time.Duration
	SnapshotSyncInterval time.Duration
	// MaxConcurrentStandbys caps how many standby operations run at once.
	// Standby writes the VM's memory snapshot to disk, so the cap bounds
	// concurrent snapshot IO. Defaults to 16 when unset.
	MaxConcurrentStandbys int
}

type controllerState struct {
	instance         Instance
	compiledPolicy   *compiledPolicy
	activeInbound    map[ConnectionKey]struct{}
	idleTimeout      time.Duration
	idleSince        *time.Time
	lastInboundAt    *time.Time
	nextStandbyAt    *time.Time
	timer            *time.Timer
	reconcileTimer   *time.Timer
	standbyRequested bool
	standbyExecuting bool
}

// Controller decides when eligible instances should transition to standby.
type Controller struct {
	store   InstanceStore
	source  ConnectionSource
	log     *slog.Logger
	now     func() time.Time
	tracer  trace.Tracer
	metrics *Metrics

	reconnectDelay       time.Duration
	reconcileDelay       time.Duration
	snapshotSyncInterval time.Duration
	timerFired           chan string
	reconcileFired       chan string
	streamReady          chan ConnectionStream
	standbySlots         chan struct{}
	standbyWG            sync.WaitGroup

	mu                sync.RWMutex
	states            map[string]*controllerState
	standbyInFlight   int
	observerConnected bool
	lastObserverErr   error
}

// NewController creates a new event-driven auto-standby controller.
func NewController(store InstanceStore, source ConnectionSource, opts ControllerOptions) *Controller {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	reconnectDelay := opts.ReconnectDelay
	if reconnectDelay <= 0 {
		reconnectDelay = defaultReconnectDelay
	}
	reconcileDelay := opts.ReconcileDelay
	if reconcileDelay <= 0 {
		reconcileDelay = defaultReconcileDelay
	}
	snapshotSyncInterval := opts.SnapshotSyncInterval
	if snapshotSyncInterval <= 0 {
		snapshotSyncInterval = defaultSnapshotSyncInterval
	}
	maxConcurrentStandbys := opts.MaxConcurrentStandbys
	if maxConcurrentStandbys <= 0 {
		maxConcurrentStandbys = defaultMaxConcurrentStandbys
	}

	c := &Controller{
		store:                store,
		source:               source,
		log:                  log,
		now:                  now,
		tracer:               opts.Tracer,
		reconnectDelay:       reconnectDelay,
		reconcileDelay:       reconcileDelay,
		snapshotSyncInterval: snapshotSyncInterval,
		timerFired:           make(chan string, 128),
		reconcileFired:       make(chan string, 128),
		streamReady:          make(chan ConnectionStream, 4),
		standbySlots:         make(chan struct{}, maxConcurrentStandbys),
		states:               make(map[string]*controllerState),
	}
	c.metrics = newMetrics(opts.Meter, opts.Tracer, c)
	return c
}

// Run starts the controller and blocks until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	log := c.log.With("tracking_mode", trackingModeConntrackEventsV4TCP)
	log.Info("auto-standby controller started", "snapshot_sync_interval", c.snapshotSyncInterval, "max_concurrent_standbys", cap(c.standbySlots))

	// Standby workers outlive individual loop iterations; cancel them when the
	// loop exits for any reason (not just ctx) and wait for in-flight work.
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer c.standbyWG.Wait()
	defer cancelWorkers()

	var stream ConnectionStream
	if c.source != nil {
		initialStream, err := c.source.OpenStream(ctx)
		if err != nil {
			c.setObserverError(err)
			c.recordObserverError("connect")
			log.Warn("auto-standby conntrack subscription failed", "error", err)
			go c.reconnectStream(ctx)
		} else {
			stream = initialStream
			c.setObserverConnected(true)
		}
	}

	if err := c.startupResync(ctx); err != nil {
		c.recordControllerError("startup_resync")
		c.log.Warn("auto-standby startup resync failed", "error", err)
	}

	instanceEvents, unsubscribe, err := c.store.SubscribeInstanceEvents()
	if err != nil {
		return err
	}
	defer unsubscribe()
	defer c.stopAllTimers()
	if stream != nil {
		defer stream.Close()
	}
	snapshotTicker := time.NewTicker(c.snapshotSyncInterval)
	defer snapshotTicker.Stop()

	for {
		var streamEvents <-chan ConnectionEvent
		var streamErrors <-chan error
		if stream != nil {
			streamEvents = stream.Events()
			streamErrors = stream.Errors()
		}

		select {
		case <-ctx.Done():
			return nil
		case replacement := <-c.streamReady:
			if stream != nil {
				_ = stream.Close()
			}
			stream = replacement
			c.setObserverConnected(true)
			log.Info("auto-standby conntrack subscription restored")
		case err := <-streamErrors:
			if err == nil {
				continue
			}
			c.setObserverError(err)
			c.recordObserverError("stream")
			log.Warn("auto-standby conntrack subscription failed", "error", err)
			if stream != nil {
				_ = stream.Close()
				stream = nil
			}
			go c.reconnectStream(ctx)
		case event, ok := <-streamEvents:
			if !ok {
				if stream != nil {
					_ = stream.Close()
					stream = nil
				}
				c.setObserverError(errors.New("conntrack stream closed"))
				c.recordObserverError("stream_closed")
				go c.reconnectStream(ctx)
				continue
			}
			c.handleConnectionEvent(ctx, event)
		case event, ok := <-instanceEvents:
			if !ok {
				return nil
			}
			if err := c.handleInstanceEvent(ctx, event); err != nil {
				c.recordObserverError("instance_event")
				log.Warn("auto-standby instance event handling failed", "action", event.Action, "instance_id", event.InstanceID, "error", err)
			}
		case id := <-c.timerFired:
			c.handleStandbyTimer(workerCtx, id)
		case id := <-c.reconcileFired:
			c.handleActiveReconcile(ctx, id)
		case <-snapshotTicker.C:
			if err := c.periodicSnapshotSync(ctx); err != nil {
				c.recordControllerError("snapshot_sync")
				log.Warn("auto-standby periodic snapshot sync failed", "error", err)
			}
		}
	}
}

// Describe returns the current diagnostic view for an instance.
func (c *Controller) Describe(inst Instance) StatusSnapshot {
	snapshot := StatusSnapshot{
		Supported:    c != nil,
		Configured:   inst.AutoStandby != nil,
		Enabled:      inst.AutoStandby != nil && inst.AutoStandby.Enabled,
		TrackingMode: trackingModeConntrackEventsV4TCP,
	}
	if c == nil {
		snapshot.Status = StatusUnsupported
		snapshot.Reason = ReasonUnsupportedPlatform
		return snapshot
	}
	if inst.AutoStandby != nil {
		snapshot.IdleTimeout = inst.AutoStandby.IdleTimeout
	}

	if inst.AutoStandby == nil {
		snapshot.Status = StatusDisabled
		snapshot.Reason = ReasonPolicyMissing
		return snapshot
	}
	if !inst.AutoStandby.Enabled {
		snapshot.Status = StatusDisabled
		snapshot.Reason = ReasonPolicyDisabled
		return snapshot
	}
	if inst.State != StateRunning {
		snapshot.Status = StatusIneligible
		snapshot.Reason = ReasonInstanceNotRunning
		return snapshot
	}
	if !inst.NetworkEnabled {
		snapshot.Status = StatusIneligible
		snapshot.Reason = ReasonNetworkDisabled
		return snapshot
	}
	if inst.IP == "" {
		snapshot.Status = StatusIneligible
		snapshot.Reason = ReasonMissingIP
		return snapshot
	}
	if inst.HasVGPU {
		snapshot.Status = StatusIneligible
		snapshot.Reason = ReasonHasVGPU
		return snapshot
	}

	snapshot.Eligible = true

	var (
		activeInboundCount int
		idleSince          *time.Time
		lastInboundAt      *time.Time
		nextStandbyAt      *time.Time
		standbyRequested   bool
		hasState           bool
	)

	c.mu.RLock()
	state := c.states[inst.ID]
	observerConnected := c.observerConnected
	lastObserverErr := c.lastObserverErr
	if state != nil {
		hasState = true
		activeInboundCount = len(state.activeInbound)
		idleSince = cloneTimePtr(state.idleSince)
		lastInboundAt = cloneTimePtr(state.lastInboundAt)
		nextStandbyAt = cloneTimePtr(state.nextStandbyAt)
		standbyRequested = state.standbyRequested
	}
	c.mu.RUnlock()

	if hasState {
		snapshot.ActiveInboundCount = activeInboundCount
		snapshot.IdleSince = idleSince
		snapshot.LastInboundActivityAt = lastInboundAt
		snapshot.NextStandbyAt = nextStandbyAt
		if nextStandbyAt != nil {
			remaining := nextStandbyAt.Sub(c.now().UTC())
			if remaining < 0 {
				remaining = 0
			}
			snapshot.CountdownRemaining = &remaining
		}
		if standbyRequested {
			snapshot.Status = StatusStandbyRequested
			snapshot.Reason = ReasonReadyForStandby
			return snapshot
		}
	}

	if !observerConnected && lastObserverErr != nil {
		snapshot.Status = StatusError
		snapshot.Reason = ReasonObserverError
		return snapshot
	}
	if hasState && activeInboundCount > 0 {
		snapshot.Status = StatusActive
		snapshot.Reason = ReasonActiveInbound
		return snapshot
	}
	if hasState && nextStandbyAt != nil {
		if nextStandbyAt.After(c.now().UTC()) {
			snapshot.Status = StatusIdleCountdown
			snapshot.Reason = ReasonIdleTimeoutNotElapsed
			return snapshot
		}
		snapshot.Status = StatusReadyForStandby
		snapshot.Reason = ReasonReadyForStandby
		return snapshot
	}

	snapshot.Status = StatusIdleCountdown
	snapshot.Reason = ReasonIdleTimeoutNotElapsed
	return snapshot
}

func (c *Controller) startupResync(ctx context.Context) error {
	start := c.now()
	ctx, span := c.startSpan(ctx, "AutoStandbyStartupResync")
	defer func() {
		if span != nil {
			span.End()
		}
	}()

	instances, err := c.store.ListInstances(ctx)
	if err != nil {
		c.recordStartupResync(start, "error")
		recordSpanError(span, err)
		return err
	}
	conns, err := c.source.ListConnections(ctx)
	if err != nil {
		c.setObserverError(err)
		c.recordObserverError("startup_resync")
		c.recordStartupResync(start, "error")
		recordSpanError(span, err)
		return err
	}

	now := c.now().UTC()
	c.log.Info("auto-standby startup resync seeded state", "instance_count", len(instances), "current_connection_count", len(conns))
	for _, inst := range instances {
		if err := c.seedInstanceState(ctx, inst, conns, now); err != nil {
			c.log.Warn("auto-standby startup resync failed for instance", "instance_id", inst.ID, "instance_name", inst.Name, "error", err)
		}
	}

	c.recordStartupResync(start, "success")
	return nil
}

func (c *Controller) periodicSnapshotSync(ctx context.Context) error {
	instances, err := c.store.ListInstances(ctx)
	if err != nil {
		return err
	}
	conns, err := c.source.ListConnections(ctx)
	if err != nil {
		return err
	}

	now := c.now().UTC()
	c.log.Debug("auto-standby periodic snapshot sync completed", "instance_count", len(instances), "current_connection_count", len(conns))
	for _, inst := range instances {
		if err := c.seedInstanceState(ctx, inst, conns, now); err != nil {
			c.log.Warn("auto-standby periodic snapshot sync failed for instance", "instance_id", inst.ID, "instance_name", inst.Name, "error", err)
		}
	}
	return nil
}

func (c *Controller) seedInstanceState(ctx context.Context, inst Instance, conns []Connection, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.refreshInstanceLocked(ctx, inst, conns, now)
}

func (c *Controller) handleInstanceEvent(ctx context.Context, event InstanceEvent) error {
	if event.Action == InstanceEventDelete {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.removeStateLocked(event.InstanceID)
		return nil
	}
	if event.Instance == nil {
		return nil
	}

	conns, err := c.source.ListConnections(ctx)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshInstanceLocked(ctx, *event.Instance, conns, c.now().UTC())
}

func (c *Controller) refreshInstanceLocked(ctx context.Context, inst Instance, conns []Connection, now time.Time) error {
	state := c.ensureStateLocked(inst.ID)
	state.instance = cloneInstance(inst)

	if !eligible(inst) {
		hadRuntime := inst.Runtime != nil || state.idleSince != nil || state.lastInboundAt != nil
		c.clearStateLocked(state)
		if hadRuntime {
			return c.persistRuntime(ctx, inst.ID, nil)
		}
		return nil
	}

	compiled, err := compilePolicy(inst.AutoStandby)
	if err != nil {
		return err
	}
	state.compiledPolicy = compiled
	state.idleTimeout = compiled.idleTimeout

	activeSet, err := matchingConnections(inst, compiled, conns)
	if err != nil {
		return err
	}
	// Cancel any queued standby attempt only once the refresh is guaranteed to
	// re-establish a countdown or reconcile below; an erroring refresh above
	// leaves the queued attempt to proceed on the last known idle state.
	state.standbyRequested = false
	state.activeInbound = activeSet

	runtime := cloneRuntime(inst.Runtime)
	if len(activeSet) > 0 {
		state.idleSince = nil
		if runtime != nil && runtime.LastInboundActivityAt != nil {
			state.lastInboundAt = cloneTimePtr(runtime.LastInboundActivityAt)
		} else {
			state.lastInboundAt = &now
		}
		c.cancelTimerLocked(state)
		c.armReconcileLocked(inst.ID, state)
		return c.persistRuntime(ctx, inst.ID, &Runtime{
			LastInboundActivityAt: cloneTimePtr(state.lastInboundAt),
		})
	}

	if runtime != nil && runtime.IdleSince != nil {
		state.idleSince = cloneTimePtr(runtime.IdleSince)
		state.lastInboundAt = cloneTimePtr(runtime.LastInboundActivityAt)
	} else {
		state.idleSince = &now
		if runtime != nil {
			state.lastInboundAt = cloneTimePtr(runtime.LastInboundActivityAt)
		} else {
			state.lastInboundAt = nil
		}
		runtime = &Runtime{
			IdleSince:             cloneTimePtr(state.idleSince),
			LastInboundActivityAt: cloneTimePtr(state.lastInboundAt),
		}
		// Persist failures must not strand the instance without a countdown;
		// the runtime only matters for recovery across controller restarts.
		if err := c.persistRuntime(ctx, inst.ID, runtime); err != nil {
			c.recordControllerError("persist_runtime")
			c.log.Warn("auto-standby failed to persist runtime during refresh", "instance_id", inst.ID, "error", err)
		}
	}
	c.armTimerLocked(inst.ID, state, now)
	return nil
}

func (c *Controller) handleConnectionEvent(ctx context.Context, event ConnectionEvent) {
	ctx, span := c.startSpan(ctx, "AutoStandbyHandleConntrackEvent",
		attribute.String("event", string(event.Type)),
	)
	defer func() {
		if span != nil {
			span.End()
		}
	}()

	if event.ObservedAt.IsZero() {
		event.ObservedAt = c.now().UTC()
	}
	c.recordConntrackEvent(string(event.Type), "received")

	c.mu.Lock()
	defer c.mu.Unlock()

	for id, state := range c.states {
		if state.compiledPolicy == nil {
			continue
		}
		key := connectionKey(event.Connection)
		matches := matchesInboundConnectionForEvent(state.instance, state.compiledPolicy, event.Connection)
		switch event.Type {
		case ConnectionEventNew:
			if !matches {
				continue
			}
			if !event.Connection.TCPState.Active() {
				if _, ok := state.activeInbound[key]; !ok {
					continue
				}
				delete(state.activeInbound, key)
				if len(state.activeInbound) > 0 {
					c.armReconcileLocked(id, state)
					continue
				}
				idleSince := event.ObservedAt.UTC()
				state.idleSince = &idleSince
				state.standbyRequested = false
				c.cancelReconcileLocked(state)
				c.armTimerLocked(id, state, idleSince)
				if err := c.persistRuntime(ctx, id, &Runtime{
					IdleSince:             cloneTimePtr(state.idleSince),
					LastInboundActivityAt: cloneTimePtr(state.lastInboundAt),
				}); err != nil {
					c.recordControllerError("persist_runtime")
					c.log.Warn("auto-standby failed to persist runtime when idle countdown started", "instance_id", id, "error", err)
				}
				c.log.Info("auto-standby idle countdown started", "instance_id", id, "idle_timeout", state.idleTimeout)
				continue
			}
			if state.activeInbound == nil {
				state.activeInbound = make(map[ConnectionKey]struct{})
			}
			state.activeInbound[key] = struct{}{}
			state.idleSince = nil
			state.lastInboundAt = cloneTimePtr(&event.ObservedAt)
			state.standbyRequested = false
			c.cancelTimerLocked(state)
			c.armReconcileLocked(id, state)
			if err := c.persistRuntime(ctx, id, &Runtime{
				LastInboundActivityAt: cloneTimePtr(state.lastInboundAt),
			}); err != nil {
				c.recordControllerError("persist_runtime")
				c.log.Warn("auto-standby failed to persist runtime after inbound activity", "instance_id", id, "error", err)
			}
			c.log.Info("auto-standby inbound activity observed", "instance_id", id, "active_inbound_connections", len(state.activeInbound))
		case ConnectionEventDestroy:
			if _, ok := state.activeInbound[key]; !ok {
				continue
			}
			delete(state.activeInbound, key)
			if len(state.activeInbound) > 0 {
				c.armReconcileLocked(id, state)
				continue
			}
			idleSince := event.ObservedAt.UTC()
			state.idleSince = &idleSince
			state.standbyRequested = false
			c.cancelReconcileLocked(state)
			c.armTimerLocked(id, state, idleSince)
			if err := c.persistRuntime(ctx, id, &Runtime{
				IdleSince:             cloneTimePtr(state.idleSince),
				LastInboundActivityAt: cloneTimePtr(state.lastInboundAt),
			}); err != nil {
				c.recordControllerError("persist_runtime")
				c.log.Warn("auto-standby failed to persist runtime when idle countdown started", "instance_id", id, "error", err)
			}
			c.log.Info("auto-standby idle countdown started", "instance_id", id, "idle_timeout", state.idleTimeout)
		}
	}
}

// handleStandbyTimer marks the instance as standby-requested and hands the
// standby call to a bounded worker so the event loop never blocks on snapshot
// IO. Inbound activity observed while the work is queued clears
// standbyRequested and cancels the attempt.
func (c *Controller) handleStandbyTimer(ctx context.Context, id string) {
	c.mu.Lock()
	state := c.states[id]
	if state == nil || state.compiledPolicy == nil || len(state.activeInbound) > 0 || state.standbyRequested || state.standbyExecuting {
		c.mu.Unlock()
		return
	}
	state.timer = nil
	state.nextStandbyAt = nil
	state.standbyRequested = true
	instanceName := state.instance.Name
	c.mu.Unlock()

	c.log.Info("auto-standby standby timer fired", "instance_id", id, "instance_name", instanceName)

	c.standbyWG.Add(1)
	go func() {
		defer c.standbyWG.Done()
		select {
		case c.standbySlots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-c.standbySlots }()
		c.executeStandby(ctx, id, instanceName)
	}()
}

func (c *Controller) executeStandby(ctx context.Context, id string, instanceName string) {
	// The slot may have been acquired in the same instant shutdown began;
	// never start a new standby after cancellation.
	if ctx.Err() != nil {
		return
	}

	ctx, span := c.startSpan(ctx, "AutoStandbyStandbyAttempt",
		attribute.String("instance_id", id),
	)
	defer func() {
		if span != nil {
			span.End()
		}
	}()

	c.mu.Lock()
	state := c.states[id]
	if state == nil || state.compiledPolicy == nil || len(state.activeInbound) > 0 || !state.standbyRequested || state.standbyExecuting {
		if state != nil && len(state.activeInbound) > 0 {
			state.standbyRequested = false
		}
		c.mu.Unlock()
		return
	}
	// standbyExecuting survives state refreshes (which reset standbyRequested),
	// so resyncs racing a running standby cannot dispatch a duplicate worker.
	state.standbyExecuting = true
	idleTimeout := state.idleTimeout
	c.standbyInFlight++
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if state := c.states[id]; state != nil {
			state.standbyExecuting = false
		}
		c.standbyInFlight--
		c.mu.Unlock()
	}()

	// activeInbound is built from conntrack events, and armReconcileLocked stops
	// re-reading the host table once it drains, so nothing has compared that view
	// against reality since the countdown started. Confirm before pausing:
	// suspending a guest that still has a live inbound flow strands that flow,
	// because wake-on-traffic does not exist.
	conns, err := c.source.ListConnections(ctx)
	if err != nil {
		// A host table we cannot read is a transient failure, and suspending
		// blind is what strands connections. Re-arm and try again next cycle.
		recordSpanError(span, err)
		c.recordStandbyAttempt("preflight_error")
		c.recordControllerError("standby_preflight")
		c.log.Warn("auto-standby could not read inbound connections before standby, deferring", "instance_id", id, "instance_name", instanceName, "error", err)

		c.mu.Lock()
		defer c.mu.Unlock()
		if state := c.states[id]; state != nil {
			state.standbyRequested = false
			idleSince := c.now().UTC()
			state.idleSince = &idleSince
			c.armTimerLocked(id, state, idleSince)
		}
		return
	}

	live, err := c.matchingInboundConnections(id, conns)
	if err != nil {
		// Classification depends on the instance record, not on host state, so
		// retrying cannot help and blocking standby forever would leak the VM.
		c.recordControllerError("standby_preflight")
		c.log.Warn("auto-standby could not classify inbound connections before standby", "instance_id", id, "instance_name", instanceName, "error", err)
	}
	if len(live) > 0 {
		c.recordStandbyAttempt("aborted_inbound_active")
		c.log.Info("auto-standby aborted, inbound connection live at standby time", "instance_id", id, "instance_name", instanceName, "active_inbound_connections", len(live))

		c.mu.Lock()
		defer c.mu.Unlock()
		if state := c.states[id]; state != nil {
			state.activeInbound = live
			state.idleSince = nil
			state.standbyRequested = false
			c.cancelTimerLocked(state)
			c.armReconcileLocked(id, state)
		}
		return
	}

	if err := c.store.StandbyInstance(ctx, id); err != nil {
		recordSpanError(span, err)
		c.recordStandbyAttempt("error")
		c.recordControllerError("standby")

		c.mu.Lock()
		defer c.mu.Unlock()
		if errors.Is(err, ErrInstanceNotFound) {
			c.log.Info("auto-standby target instance no longer exists, dropping state", "instance_id", id, "instance_name", instanceName)
			c.removeStateLocked(id)
			return
		}
		c.log.Warn("auto-standby standby attempt failed", "instance_id", id, "instance_name", instanceName, "error", err)
		if state := c.states[id]; state != nil {
			state.standbyRequested = false
			// Inbound activity that arrived during the attempt owns the state
			// now; the reconcile/destroy flow restarts the countdown once the
			// connections drain.
			if len(state.activeInbound) > 0 {
				return
			}
			idleSince := c.now().UTC()
			state.idleSince = &idleSince
			c.armTimerLocked(id, state, idleSince)
			if persistErr := c.persistRuntime(ctx, id, &Runtime{
				IdleSince:             cloneTimePtr(state.idleSince),
				LastInboundActivityAt: cloneTimePtr(state.lastInboundAt),
			}); persistErr != nil {
				c.recordControllerError("persist_runtime")
				c.log.Warn("auto-standby failed to persist runtime after standby failure", "instance_id", id, "error", persistErr)
			}
		}
		return
	}

	c.log.Info("instance entered standby due to inbound inactivity", "instance_id", id, "instance_name", instanceName, "idle_timeout", idleTimeout)

	c.mu.Lock()
	// Activity observed while the snapshot was in flight is about to be dropped
	// by clearStateLocked, which would leave the guest suspended with a live
	// connection attached to it. The failing path above hands that state back to
	// the reconcile flow; a successful standby has to undo itself instead.
	raced := false
	if state := c.states[id]; state != nil {
		raced = len(state.activeInbound) > 0
		c.clearStateLocked(state)
		if err := c.persistRuntime(ctx, id, nil); err != nil {
			c.recordControllerError("persist_runtime")
			c.log.Warn("auto-standby failed to clear runtime after standby", "instance_id", id, "error", err)
		}
	}
	c.mu.Unlock()

	if raced {
		c.recordStandbyAttempt("raced_restored")
		c.restoreAfterRacedStandby(ctx, id, instanceName)
		return
	}
	c.recordStandbyAttempt("success")
}

// restoreAfterRacedStandby wakes an instance that reached standby while inbound
// activity was arriving. The connection that raced the snapshot is already
// pointed at the suspended guest, and nothing else will wake it.
func (c *Controller) restoreAfterRacedStandby(ctx context.Context, id string, instanceName string) {
	c.log.Warn("inbound activity raced standby, restoring instance", "instance_id", id, "instance_name", instanceName)

	if err := c.store.RestoreInstance(ctx, id); err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.removeStateLocked(id)
			return
		}
		c.recordControllerError("restore_after_raced_standby")
		c.log.Error("failed to restore instance after standby raced inbound activity", "instance_id", id, "instance_name", instanceName, "error", err)
	}
}

// matchingInboundConnections returns the flows from conns that match the
// instance's policy.
func (c *Controller) matchingInboundConnections(id string, conns []Connection) (map[ConnectionKey]struct{}, error) {
	c.mu.Lock()
	state := c.states[id]
	if state == nil || state.compiledPolicy == nil {
		c.mu.Unlock()
		return nil, nil
	}
	inst := cloneInstance(state.instance)
	policy := state.compiledPolicy
	c.mu.Unlock()

	return matchingConnections(inst, policy, conns)
}

func (c *Controller) handleActiveReconcile(ctx context.Context, id string) {
	conns, err := c.source.ListConnections(ctx)
	if err != nil {
		c.recordControllerError("reconcile")
		c.log.Warn("auto-standby active connection reconcile failed", "instance_id", id, "error", err)

		c.mu.Lock()
		defer c.mu.Unlock()
		if state := c.states[id]; state != nil && len(state.activeInbound) > 0 {
			c.armReconcileLocked(id, state)
		}
		return
	}

	now := c.now().UTC()

	c.mu.Lock()
	defer c.mu.Unlock()

	state := c.states[id]
	if state == nil || state.compiledPolicy == nil {
		return
	}

	activeSet, err := matchingConnections(state.instance, state.compiledPolicy, conns)
	if err != nil {
		c.recordControllerError("reconcile")
		c.log.Warn("auto-standby active connection reconcile failed to classify connections", "instance_id", id, "error", err)
		if len(state.activeInbound) > 0 {
			c.armReconcileLocked(id, state)
		}
		return
	}

	state.activeInbound = activeSet
	if len(activeSet) > 0 {
		state.standbyRequested = false
		c.armReconcileLocked(id, state)
		return
	}

	state.idleSince = &now
	state.standbyRequested = false
	c.cancelReconcileLocked(state)
	c.armTimerLocked(id, state, now)
	if err := c.persistRuntime(ctx, id, &Runtime{
		IdleSince:             cloneTimePtr(state.idleSince),
		LastInboundActivityAt: cloneTimePtr(state.lastInboundAt),
	}); err != nil {
		c.recordControllerError("persist_runtime")
		c.log.Warn("auto-standby failed to persist runtime after active connection reconcile drained", "instance_id", id, "error", err)
	}
	c.log.Info("auto-standby idle countdown started after active connection reconcile", "instance_id", id, "idle_timeout", state.idleTimeout)
}

func (c *Controller) reconnectStream(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		stream, err := c.source.OpenStream(ctx)
		if err == nil {
			select {
			case c.streamReady <- stream:
			case <-ctx.Done():
				_ = stream.Close()
			}
			return
		}

		c.setObserverError(err)
		c.recordObserverError("reconnect")
		c.log.Warn("auto-standby conntrack subscription reconnect failed", "error", err)

		timer := time.NewTimer(c.reconnectDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (c *Controller) ensureStateLocked(id string) *controllerState {
	state, ok := c.states[id]
	if !ok {
		state = &controllerState{}
		c.states[id] = state
	}
	return state
}

func (c *Controller) removeStateLocked(id string) {
	state := c.states[id]
	if state != nil {
		c.cancelTimerLocked(state)
		c.cancelReconcileLocked(state)
	}
	delete(c.states, id)
}

func (c *Controller) clearStateLocked(state *controllerState) {
	state.compiledPolicy = nil
	state.activeInbound = nil
	state.idleTimeout = 0
	state.idleSince = nil
	state.lastInboundAt = nil
	state.nextStandbyAt = nil
	state.standbyRequested = false
	c.cancelTimerLocked(state)
	c.cancelReconcileLocked(state)
}

func (c *Controller) armTimerLocked(id string, state *controllerState, now time.Time) {
	if state.idleSince == nil || state.idleTimeout <= 0 {
		c.cancelTimerLocked(state)
		return
	}

	when := state.idleSince.Add(state.idleTimeout)
	delay := when.Sub(now)
	if delay < 0 {
		delay = 0
	}

	c.cancelTimerLocked(state)
	state.nextStandbyAt = &when
	c.log.Debug("auto-standby standby timer armed", "instance_id", id, "next_standby_at", when, "idle_timeout", state.idleTimeout)
	state.timer = time.AfterFunc(delay, func() {
		select {
		case c.timerFired <- id:
		default:
		}
	})
}

func (c *Controller) cancelTimerLocked(state *controllerState) {
	if state.timer != nil {
		state.timer.Stop()
		c.log.Debug("auto-standby standby timer cancelled", "instance_id", state.instance.ID)
		state.timer = nil
	}
	state.nextStandbyAt = nil
}

func (c *Controller) armReconcileLocked(id string, state *controllerState) {
	if len(state.activeInbound) == 0 || c.reconcileDelay <= 0 {
		c.cancelReconcileLocked(state)
		return
	}

	c.cancelReconcileLocked(state)
	state.reconcileTimer = time.AfterFunc(c.reconcileDelay, func() {
		select {
		case c.reconcileFired <- id:
		default:
		}
	})
}

func (c *Controller) cancelReconcileLocked(state *controllerState) {
	if state.reconcileTimer != nil {
		state.reconcileTimer.Stop()
		state.reconcileTimer = nil
	}
}

func (c *Controller) stopAllTimers() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, state := range c.states {
		c.cancelTimerLocked(state)
		c.cancelReconcileLocked(state)
	}
}

func (c *Controller) persistRuntime(ctx context.Context, id string, runtime *Runtime) error {
	return c.store.SetRuntime(ctx, id, runtime)
}

func (c *Controller) setObserverConnected(connected bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observerConnected = connected
	if connected {
		c.lastObserverErr = nil
	}
}

func (c *Controller) setObserverError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observerConnected = false
	c.lastObserverErr = err
}

func matchingConnections(inst Instance, compiled *compiledPolicy, conns []Connection) (map[ConnectionKey]struct{}, error) {
	instanceIP, err := netip.ParseAddr(inst.IP)
	if err != nil {
		return nil, fmt.Errorf("parse instance IP %q: %w", inst.IP, err)
	}

	out := make(map[ConnectionKey]struct{})
	for _, conn := range conns {
		if matchesInboundConnection(instanceIP, compiled, conn) {
			out[connectionKey(conn)] = struct{}{}
		}
	}
	return out, nil
}

func matchesInboundConnectionForEvent(inst Instance, policy *compiledPolicy, conn Connection) bool {
	instanceIP, err := netip.ParseAddr(inst.IP)
	if err != nil {
		return false
	}
	if !conn.OriginalDestinationIP.IsValid() || conn.OriginalDestinationIP != instanceIP {
		return false
	}
	if _, ignored := policy.ignorePorts[conn.OriginalDestinationPort]; ignored {
		return false
	}
	if !conn.OriginalSourceIP.IsValid() {
		return false
	}
	for _, prefix := range policy.ignoreSourceCIDRs {
		if prefix.Contains(conn.OriginalSourceIP) {
			return false
		}
	}
	return true
}

type ConnectionKey struct {
	OriginalSourceIP        netip.Addr
	OriginalSourcePort      uint16
	OriginalDestinationIP   netip.Addr
	OriginalDestinationPort uint16
}

func connectionKey(conn Connection) ConnectionKey {
	return ConnectionKey{
		OriginalSourceIP:        conn.OriginalSourceIP,
		OriginalSourcePort:      conn.OriginalSourcePort,
		OriginalDestinationIP:   conn.OriginalDestinationIP,
		OriginalDestinationPort: conn.OriginalDestinationPort,
	}
}

func cloneRuntime(runtime *Runtime) *Runtime {
	if runtime == nil {
		return nil
	}
	return &Runtime{
		IdleSince:             cloneTimePtr(runtime.IdleSince),
		LastInboundActivityAt: cloneTimePtr(runtime.LastInboundActivityAt),
	}
}

func cloneInstance(inst Instance) Instance {
	cloned := inst
	cloned.AutoStandby = clonePolicy(inst.AutoStandby)
	cloned.Runtime = cloneRuntime(inst.Runtime)
	return cloned
}

func clonePolicy(policy *Policy) *Policy {
	if policy == nil {
		return nil
	}
	out := &Policy{
		Enabled:     policy.Enabled,
		IdleTimeout: policy.IdleTimeout,
	}
	if len(policy.IgnoreSourceCIDRs) > 0 {
		out.IgnoreSourceCIDRs = append([]string(nil), policy.IgnoreSourceCIDRs...)
	}
	if len(policy.IgnoreDestinationPorts) > 0 {
		out.IgnoreDestinationPorts = append([]uint16(nil), policy.IgnoreDestinationPorts...)
	}
	return out
}

func cloneTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	copied := t.UTC()
	return &copied
}

func eligible(inst Instance) bool {
	if inst.State != StateRunning {
		return false
	}
	if !inst.NetworkEnabled || inst.IP == "" || inst.HasVGPU {
		return false
	}
	return inst.AutoStandby != nil && inst.AutoStandby.Enabled
}

func (c *Controller) startSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if c.tracer == nil {
		return ctx, nil
	}
	return c.tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}

func recordSpanError(span trace.Span, err error) {
	if span == nil || err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
