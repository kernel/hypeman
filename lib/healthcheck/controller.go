package healthcheck

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const defaultTimerBufferSize = 256

type ControllerOptions struct {
	Log *slog.Logger
	Now func() time.Time
}

type controllerState struct {
	instance Instance
	policy   *Policy
	timer    *time.Timer
}

type Controller struct {
	store  InstanceStore
	runner ProbeRunner
	log    *slog.Logger
	now    func() time.Time

	timerFired chan string

	mu     sync.Mutex
	states map[string]*controllerState
}

func NewController(store InstanceStore, runner ProbeRunner, opts ControllerOptions) *Controller {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if runner == nil {
		runner = DefaultProbeRunner{}
	}

	return &Controller{
		store:      store,
		runner:     runner,
		log:        log,
		now:        now,
		timerFired: make(chan string, defaultTimerBufferSize),
		states:     make(map[string]*controllerState),
	}
}

func (c *Controller) Run(ctx context.Context) error {
	c.log.Info("health check controller started")

	if err := c.startupResync(ctx); err != nil {
		c.log.Warn("health check startup resync failed", "error", err)
	}

	events, unsubscribe, err := c.store.SubscribeInstanceEvents()
	if err != nil {
		return err
	}
	defer unsubscribe()
	defer c.stopAllTimers()

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-events:
			if !ok {
				return nil
			}
			c.handleInstanceEvent(ctx, event)
		case id := <-c.timerFired:
			c.runCheck(ctx, id)
		}
	}
}

func (c *Controller) startupResync(ctx context.Context) error {
	insts, err := c.store.ListInstances(ctx)
	if err != nil {
		return err
	}
	for _, inst := range insts {
		c.syncInstance(ctx, &inst, false, false)
	}
	return nil
}

func (c *Controller) handleInstanceEvent(ctx context.Context, event InstanceEvent) {
	if event.Action == InstanceEventDelete || event.Instance == nil {
		c.removeInstance(event.InstanceID)
		return
	}
	resetRuntime := event.Action == InstanceEventStart || event.Action == InstanceEventRestore
	c.syncInstance(ctx, event.Instance, true, resetRuntime)
}

func (c *Controller) syncInstance(ctx context.Context, inst *Instance, immediate bool, resetRuntime bool) {
	policy, err := NormalizePolicy(inst.HealthCheck)
	if err != nil {
		c.log.Warn("invalid health check policy", "instance_id", inst.ID, "error", err)
		c.removeInstance(inst.ID)
		return
	}
	if !Enabled(policy) || inst.State != StateRunning {
		c.removeInstance(inst.ID)
		return
	}

	interval, _, _, err := DurationConfig(policy)
	if err != nil {
		c.log.Warn("invalid health check duration", "instance_id", inst.ID, "error", err)
		c.removeInstance(inst.ID)
		return
	}

	runtime := CloneRuntime(inst.Runtime)
	if resetRuntime || runtime == nil {
		runtime = initialRuntime(inst, c.now())
		if err := c.store.SetRuntime(ctx, inst.ID, runtime); err != nil {
			c.log.Warn("failed to persist initial health check status", "instance_id", inst.ID, "error", err)
		}
	}

	delay := interval
	if immediate || runtime.LastCheckedAt == nil {
		delay = 0
	}

	instCopy := *inst
	instCopy.Runtime = runtime

	c.mu.Lock()
	state, ok := c.states[inst.ID]
	if !ok {
		state = &controllerState{}
		c.states[inst.ID] = state
	}
	state.instance = instCopy
	state.policy = policy
	c.scheduleLocked(inst.ID, state, delay)
	c.mu.Unlock()
}

func (c *Controller) removeInstance(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if state := c.states[id]; state != nil && state.timer != nil {
		state.timer.Stop()
	}
	delete(c.states, id)
}

func (c *Controller) runCheck(ctx context.Context, id string) {
	c.mu.Lock()
	state := c.states[id]
	if state == nil {
		c.mu.Unlock()
		return
	}
	inst := state.instance
	policy := ClonePolicy(state.policy)
	previous := CloneRuntime(inst.Runtime)
	c.mu.Unlock()

	_, timeout, _, err := DurationConfig(policy)
	if err != nil {
		c.log.Warn("invalid health check duration", "instance_id", id, "error", err)
		c.removeInstance(id)
		return
	}

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	result := c.runner.Check(checkCtx, inst, policy)
	cancel()

	runtime := ApplyProbeResult(policy, inst, previous, c.now(), result)
	if err := c.store.SetRuntime(ctx, id, runtime); err != nil {
		c.log.Warn("failed to persist health check status", "instance_id", id, "error", err)
	}

	interval, _, _, err := DurationConfig(policy)
	if err != nil {
		c.log.Warn("invalid health check interval", "instance_id", id, "error", err)
		c.removeInstance(id)
		return
	}

	c.mu.Lock()
	state = c.states[id]
	if state != nil {
		state.instance.Runtime = runtime
		c.scheduleLocked(id, state, interval)
	}
	c.mu.Unlock()
}

func (c *Controller) scheduleLocked(id string, state *controllerState, delay time.Duration) {
	if state.timer != nil {
		state.timer.Stop()
	}
	state.timer = time.AfterFunc(delay, func() {
		select {
		case c.timerFired <- id:
		default:
			c.log.Warn("dropped health check timer event", "instance_id", id)
		}
	})
}

func (c *Controller) stopAllTimers() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, state := range c.states {
		if state.timer != nil {
			state.timer.Stop()
		}
	}
	c.states = make(map[string]*controllerState)
}

func initialRuntime(inst *Instance, now time.Time) *Runtime {
	now = now.UTC()
	startedAt := now
	if inst.StartedAt != nil {
		startedAt = inst.StartedAt.UTC()
	}
	return &Runtime{
		Status:    StatusStarting,
		StartedAt: &startedAt,
	}
}
