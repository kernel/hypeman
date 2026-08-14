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

// ErrStandbyInProgress signals that a standby operation is already executing
// and can no longer be cancelled.
var ErrStandbyInProgress = errors.New("standby in progress")

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
	holdUntil        time.Time
	timer            *time.Timer
	reconcileTimer   *time.Timer
	standbyRequested bool
	standbyExecuting bool
}

// runtimePersistence preserves controller mutation order while metadata writes
// run without holding the controller mutex.
type runtimePersistence struct {
	id         string
	runtime    *Runtime
	generation uint64
	bestEffort bool
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

	mu                    sync.RWMutex
	states                map[string]*controllerState
	runtimeGenerations    map[string]uint64
	nextRuntimeGeneration uint64
	standbyInFlight       int
	observerConnected     bool
	lastObserverErr       error

	runtimePersistMu sync.Mutex
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
		runtimeGenerations:   make(map[string]uint64),
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
			log.Info("auto-standby conntrack subscription restored")
			// Events published during the outage are gone, so reseed from the
			// conntrack table instead of waiting for the next snapshot sync.
			if err := c.startupResync(ctx); err != nil {
				c.recordControllerError("startup_resync")
				log.Warn("auto-standby resync after subscription restore failed", "error", err)
			}
			// Marked connected after the reseed: a failed conntrack dump inside
			// it reports an observer error, which would leave the restored
			// subscription looking dead until the next reconnect.
			c.setObserverConnected(true)
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
		holdUntil          time.Time
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
		holdUntil = state.holdUntil
		standbyRequested = state.standbyRequested
	}
	c.mu.RUnlock()

	if hasState {
		snapshot.ActiveInboundCount = activeInboundCount
		snapshot.IdleSince = idleSince
		snapshot.LastInboundActivityAt = lastInboundAt
		snapshot.NextStandbyAt = nextStandbyAt
		if holdUntil.After(c.now().UTC()) {
			snapshot.HoldUntil = cloneTimePtr(&holdUntil)
		}
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

// HoldStandby guarantees the controller will not put the instance into
// standby before now + the policy idle timeout, and cancels any queued
// standby attempt. The idle countdown itself is untouched. Each hold replaces
// the instance's previous one, so a hold placed under a shorter idle timeout
// moves an earlier, longer deadline in. It fails with ErrStandbyInProgress once
// a standby operation is executing, and is a no-op for instances that cannot
// auto-standby at all (unconfigured, disabled, or ineligible).
func (c *Controller) HoldStandby(_ context.Context, inst Instance) (StatusSnapshot, error) {
	now := c.now().UTC()

	c.mu.Lock()
	state := c.states[inst.ID]
	if state != nil && state.standbyExecuting {
		c.mu.Unlock()
		return StatusSnapshot{}, ErrStandbyInProgress
	}

	idleTimeout := time.Duration(0)
	if state != nil && state.compiledPolicy != nil {
		idleTimeout = state.idleTimeout
	} else if eligible(inst) {
		compiled, err := compilePolicy(inst.AutoStandby)
		if err != nil {
			c.mu.Unlock()
			return StatusSnapshot{}, err
		}
		idleTimeout = compiled.idleTimeout
		// The instance may not be seeded yet (e.g. a hold racing controller
		// startup); record the hold on a bare state so the seeding resync
		// arms its timer no earlier than hold_until.
		state = c.ensureStateLocked(inst.ID)
	}
	if idleTimeout <= 0 {
		c.mu.Unlock()
		return c.Describe(inst), nil
	}

	// The newest hold wins. A caller holding under a shorter idle timeout is
	// asking for a shorter deadline, and there is no release endpoint, so
	// keeping the earlier longer one would pin the instance awake until it
	// lapsed on its own.
	holdUntil := now.Add(idleTimeout)
	state.holdUntil = holdUntil
	state.standbyRequested = false
	if state.compiledPolicy != nil {
		c.armTimerLocked(inst.ID, state, now)
	}
	c.mu.Unlock()

	c.log.Info("auto-standby hold placed", "instance_id", inst.ID, "instance_name", inst.Name, "hold_until", holdUntil)
	return c.Describe(inst), nil
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
	persistence, err := c.refreshInstanceLocked(inst, conns, now)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return c.persistRuntime(ctx, persistence)
}

func (c *Controller) handleInstanceEvent(ctx context.Context, event InstanceEvent) error {
	if event.Action == InstanceEventDelete {
		c.mu.Lock()
		c.removeStateLocked(event.InstanceID)
		c.mu.Unlock()
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
	persistence, err := c.refreshInstanceLocked(*event.Instance, conns, c.now().UTC())
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return c.persistRuntime(ctx, persistence)
}

func (c *Controller) refreshInstanceLocked(inst Instance, conns []Connection, now time.Time) (runtimePersistence, error) {
	state := c.ensureStateLocked(inst.ID)
	state.instance = cloneInstance(inst)

	if !eligible(inst) {
		hadRuntime := inst.Runtime != nil || state.idleSince != nil || state.lastInboundAt != nil
		c.clearStateLocked(state)
		if hadRuntime {
			return c.prepareRuntimePersistenceLocked(inst.ID, nil, false), nil
		}
		return runtimePersistence{}, nil
	}

	compiled, err := compilePolicy(inst.AutoStandby)
	if err != nil {
		return runtimePersistence{}, err
	}
	state.compiledPolicy = compiled
	state.idleTimeout = compiled.idleTimeout

	activeSet, err := matchingConnections(inst, compiled, conns)
	if err != nil {
		return runtimePersistence{}, err
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
		return c.prepareRuntimePersistenceLocked(inst.ID, &Runtime{
			LastInboundActivityAt: cloneTimePtr(state.lastInboundAt),
		}, false), nil
	}

	var persistence runtimePersistence
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
		persistence = c.prepareRuntimePersistenceLocked(inst.ID, &Runtime{
			IdleSince:             cloneTimePtr(state.idleSince),
			LastInboundActivityAt: cloneTimePtr(state.lastInboundAt),
		}, true)
	}
	c.armTimerLocked(inst.ID, state, now)
	return persistence, nil
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
	persistences := make([]runtimePersistence, 0, 1)
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
				persistences = append(persistences, c.prepareRuntimePersistenceLocked(id, &Runtime{
					IdleSince:             cloneTimePtr(state.idleSince),
					LastInboundActivityAt: cloneTimePtr(state.lastInboundAt),
				}, false))
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
			persistences = append(persistences, c.prepareRuntimePersistenceLocked(id, &Runtime{
				LastInboundActivityAt: cloneTimePtr(state.lastInboundAt),
			}, false))
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
			persistences = append(persistences, c.prepareRuntimePersistenceLocked(id, &Runtime{
				IdleSince:             cloneTimePtr(state.idleSince),
				LastInboundActivityAt: cloneTimePtr(state.lastInboundAt),
			}, false))
			c.log.Info("auto-standby idle countdown started", "instance_id", id, "idle_timeout", state.idleTimeout)
		}
	}
	c.mu.Unlock()

	for _, persistence := range persistences {
		if err := c.persistRuntime(ctx, persistence); err != nil {
			c.recordControllerError("persist_runtime")
			c.log.Warn("auto-standby failed to persist runtime after connection event", "instance_id", persistence.id, "error", err)
		}
	}
}

// confirmIdleBeforeStandby re-reads the conntrack table and reports whether the
// instance is still idle. activeInbound is built from the event stream, so a
// dropped NEW event leaves it empty while the guest holds a live connection; the
// table is authoritative. When the instance turns out to be busy the reconcile
// loop takes the state back over, and when the table cannot be read the instance
// is treated as busy so an unconfirmed guest is never taken away.
func (c *Controller) confirmIdleBeforeStandby(ctx context.Context, id string) bool {
	conns, listErr := c.source.ListConnections(ctx)

	c.mu.Lock()
	state := c.states[id]
	// Activity can land between the timer firing and this check, and it already
	// owns idleSince and the reconcile loop; the paths below must not clobber it.
	if state == nil || state.compiledPolicy == nil || len(state.activeInbound) > 0 {
		c.mu.Unlock()
		return false
	}

	var activeSet map[ConnectionKey]struct{}
	err := listErr
	if err == nil {
		activeSet, err = matchingConnections(state.instance, state.compiledPolicy, conns)
	}
	if err != nil {
		idleSince := c.now().UTC()
		state.idleSince = &idleSince
		c.armTimerLocked(id, state, idleSince)
		persistence := c.prepareRuntimePersistenceLocked(id, &Runtime{
			IdleSince:             cloneTimePtr(state.idleSince),
			LastInboundActivityAt: cloneTimePtr(state.lastInboundAt),
		}, false)
		c.mu.Unlock()

		if persistErr := c.persistRuntime(ctx, persistence); persistErr != nil {
			c.recordControllerError("persist_runtime")
			c.log.Warn("auto-standby failed to persist runtime after unconfirmed standby", "instance_id", id, "error", persistErr)
		}
		c.recordControllerError("standby_confirm")
		c.log.Warn("auto-standby could not confirm idle before standby", "instance_id", id, "error", err)
		return false
	}
	if len(activeSet) == 0 {
		c.mu.Unlock()
		return true
	}

	now := c.now().UTC()
	state.activeInbound = activeSet
	state.idleSince = nil
	state.lastInboundAt = &now
	c.cancelTimerLocked(state)
	c.armReconcileLocked(id, state)
	persistence := c.prepareRuntimePersistenceLocked(id, &Runtime{
		LastInboundActivityAt: cloneTimePtr(state.lastInboundAt),
	}, false)
	c.mu.Unlock()

	if err := c.persistRuntime(ctx, persistence); err != nil {
		c.recordControllerError("persist_runtime")
		c.log.Warn("auto-standby failed to persist runtime after standby confirmation found connections", "instance_id", id, "error", err)
	}
	c.log.Info("auto-standby skipped standby, conntrack still reports inbound connections", "instance_id", id, "active_inbound_connections", len(activeSet))
	return false
}

// standbyDeadlineElapsedLocked reports whether the armed standby deadline has
// actually passed. A timer callback that fired before a re-arm (hold,
// destroy-event restart) still delivers its id, so re-arm on the way out to
// replace the spent timer, including when a backward clock step makes a
// monotonic timer fire early by wall clock.
func (c *Controller) standbyDeadlineElapsedLocked(id string, state *controllerState) bool {
	now := c.now().UTC()
	if state.nextStandbyAt == nil || now.Before(*state.nextStandbyAt) {
		c.armTimerLocked(id, state, now)
		return false
	}
	return true
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
	if !c.standbyDeadlineElapsedLocked(id, state) {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	if !c.confirmIdleBeforeStandby(ctx, id) {
		return
	}

	c.mu.Lock()
	state = c.states[id]
	if state == nil || state.compiledPolicy == nil || len(state.activeInbound) > 0 || state.standbyRequested || state.standbyExecuting {
		c.mu.Unlock()
		return
	}
	// Checked again because the confirmation released the lock: a hold placed
	// while it ran pushes the deadline back out.
	if !c.standbyDeadlineElapsedLocked(id, state) {
		c.mu.Unlock()
		return
	}
	// Cancelled rather than dropped: a failing standby worker may have armed a
	// replacement timer during the confirmation.
	c.cancelTimerLocked(state)
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

	if err := c.store.StandbyInstance(ctx, id); err != nil {
		recordSpanError(span, err)
		c.recordStandbyAttempt("error")
		c.recordControllerError("standby")

		c.mu.Lock()
		if errors.Is(err, ErrInstanceNotFound) {
			c.log.Info("auto-standby target instance no longer exists, dropping state", "instance_id", id, "instance_name", instanceName)
			c.removeStateLocked(id)
			c.mu.Unlock()
			return
		}
		c.log.Warn("auto-standby standby attempt failed", "instance_id", id, "instance_name", instanceName, "error", err)
		var persistence runtimePersistence
		if state := c.states[id]; state != nil {
			state.standbyRequested = false
			// Inbound activity that arrived during the attempt owns the state
			// now; the reconcile/destroy flow restarts the countdown once the
			// connections drain.
			if len(state.activeInbound) == 0 {
				idleSince := c.now().UTC()
				state.idleSince = &idleSince
				c.armTimerLocked(id, state, idleSince)
				persistence = c.prepareRuntimePersistenceLocked(id, &Runtime{
					IdleSince:             cloneTimePtr(state.idleSince),
					LastInboundActivityAt: cloneTimePtr(state.lastInboundAt),
				}, false)
			}
		}
		c.mu.Unlock()
		if persistErr := c.persistRuntime(ctx, persistence); persistErr != nil {
			c.recordControllerError("persist_runtime")
			c.log.Warn("auto-standby failed to persist runtime after standby failure", "instance_id", id, "error", persistErr)
		}
		return
	}

	c.recordStandbyAttempt("success")
	c.log.Info("instance entered standby due to inbound inactivity", "instance_id", id, "instance_name", instanceName, "idle_timeout", idleTimeout)

	c.mu.Lock()
	var persistence runtimePersistence
	if state := c.states[id]; state != nil {
		c.clearStateLocked(state)
		persistence = c.prepareRuntimePersistenceLocked(id, nil, false)
	}
	c.mu.Unlock()
	if err := c.persistRuntime(ctx, persistence); err != nil {
		c.recordControllerError("persist_runtime")
		c.log.Warn("auto-standby failed to clear runtime after standby", "instance_id", id, "error", err)
	}
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
	state := c.states[id]
	if state == nil || state.compiledPolicy == nil {
		c.mu.Unlock()
		return
	}

	activeSet, err := matchingConnections(state.instance, state.compiledPolicy, conns)
	if err != nil {
		c.recordControllerError("reconcile")
		c.log.Warn("auto-standby active connection reconcile failed to classify connections", "instance_id", id, "error", err)
		if len(state.activeInbound) > 0 {
			c.armReconcileLocked(id, state)
		}
		c.mu.Unlock()
		return
	}

	state.activeInbound = activeSet
	if len(activeSet) > 0 {
		state.standbyRequested = false
		c.armReconcileLocked(id, state)
		c.mu.Unlock()
		return
	}

	state.idleSince = &now
	state.standbyRequested = false
	c.cancelReconcileLocked(state)
	c.armTimerLocked(id, state, now)
	persistence := c.prepareRuntimePersistenceLocked(id, &Runtime{
		IdleSince:             cloneTimePtr(state.idleSince),
		LastInboundActivityAt: cloneTimePtr(state.lastInboundAt),
	}, false)
	idleTimeout := state.idleTimeout
	c.mu.Unlock()

	if err := c.persistRuntime(ctx, persistence); err != nil {
		c.recordControllerError("persist_runtime")
		c.log.Warn("auto-standby failed to persist runtime after active connection reconcile drained", "instance_id", id, "error", err)
	}
	c.log.Info("auto-standby idle countdown started after active connection reconcile", "instance_id", id, "idle_timeout", idleTimeout)
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
	delete(c.runtimeGenerations, id)
}

func (c *Controller) clearStateLocked(state *controllerState) {
	state.compiledPolicy = nil
	state.activeInbound = nil
	state.idleTimeout = 0
	state.idleSince = nil
	state.lastInboundAt = nil
	state.nextStandbyAt = nil
	state.holdUntil = time.Time{}
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
	if state.holdUntil.After(when) {
		when = state.holdUntil
	}
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

func (c *Controller) prepareRuntimePersistenceLocked(id string, runtime *Runtime, bestEffort bool) runtimePersistence {
	c.nextRuntimeGeneration++
	generation := c.nextRuntimeGeneration
	c.runtimeGenerations[id] = generation
	return runtimePersistence{
		id:         id,
		runtime:    cloneRuntime(runtime),
		generation: generation,
		bestEffort: bestEffort,
	}
}

func (c *Controller) persistRuntime(ctx context.Context, persistence runtimePersistence) error {
	if persistence.generation == 0 {
		return nil
	}

	c.runtimePersistMu.Lock()
	defer c.runtimePersistMu.Unlock()

	c.mu.RLock()
	generation := c.runtimeGenerations[persistence.id]
	c.mu.RUnlock()
	if generation != persistence.generation {
		return nil
	}

	err := c.store.SetRuntime(ctx, persistence.id, persistence.runtime)
	if err != nil && persistence.bestEffort {
		c.recordControllerError("persist_runtime")
		c.log.Warn("auto-standby failed to persist runtime", "instance_id", persistence.id, "error", err)
		return nil
	}
	return err
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
