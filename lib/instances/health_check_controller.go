package instances

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/healthcheck"
)

const defaultHealthCheckTimerBufferSize = 256

type healthCheckControllerStore interface {
	ListInstances(ctx context.Context, filter *ListInstancesFilter) ([]Instance, error)
	SetHealthCheckRuntime(ctx context.Context, id string, runtime *healthcheck.Runtime) error
	SubscribeLifecycleEvents(consumer LifecycleEventConsumer) (<-chan LifecycleEvent, func())
}

type healthCheckControllerOptions struct {
	Log    *slog.Logger
	Now    func() time.Time
	Runner healthcheck.ProbeRunner
}

type healthCheckControllerState struct {
	instance healthcheck.Instance
	policy   *healthcheck.Policy
	timer    *time.Timer
}

type HealthCheckController struct {
	store  healthCheckControllerStore
	runner healthcheck.ProbeRunner
	log    *slog.Logger
	now    func() time.Time

	timerFired chan string

	mu     sync.Mutex
	states map[string]*healthCheckControllerState
}

func NewHealthCheckController(manager Manager, log *slog.Logger) *HealthCheckController {
	if manager == nil || log == nil {
		return nil
	}
	store, ok := manager.(healthCheckControllerStore)
	if !ok {
		return nil
	}

	return newHealthCheckController(store, healthCheckControllerOptions{
		Log:    log.With("controller", "health_check"),
		Runner: healthcheck.DefaultProbeRunner{ExecRunner: healthCheckExecRunner{manager: manager}},
	})
}

func newHealthCheckController(store healthCheckControllerStore, opts healthCheckControllerOptions) *HealthCheckController {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	runner := opts.Runner
	if runner == nil {
		runner = healthcheck.DefaultProbeRunner{}
	}

	return &HealthCheckController{
		store:      store,
		runner:     runner,
		log:        log,
		now:        now,
		timerFired: make(chan string, defaultHealthCheckTimerBufferSize),
		states:     make(map[string]*healthCheckControllerState),
	}
}

func (c *HealthCheckController) Run(ctx context.Context) error {
	c.log.Info("health check controller started")

	if err := c.startupResync(ctx); err != nil {
		c.log.Warn("health check startup resync failed", "error", err)
	}

	events, unsubscribe := c.store.SubscribeLifecycleEvents(LifecycleEventConsumerHealthCheck)
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
			c.handleInstanceEvent(event)
		case id := <-c.timerFired:
			c.runCheck(ctx, id)
		}
	}
}

func (c *HealthCheckController) startupResync(ctx context.Context) error {
	insts, err := c.store.ListInstances(ctx, nil)
	if err != nil {
		return err
	}
	for _, inst := range insts {
		c.syncInstance(&inst, false, false)
	}
	return nil
}

func (c *HealthCheckController) handleInstanceEvent(event LifecycleEvent) {
	if event.Action == LifecycleEventDelete || event.Instance == nil {
		c.removeInstance(event.InstanceID)
		return
	}
	resetRuntime := event.Action == LifecycleEventStart || event.Action == LifecycleEventRestore
	c.syncInstance(event.Instance, true, resetRuntime)
}

func (c *HealthCheckController) syncInstance(inst *Instance, immediate bool, resetRuntime bool) {
	policy, err := healthcheck.NormalizePolicy(inst.HealthCheck)
	if err != nil {
		c.log.Warn("invalid health check policy", "instance_id", inst.Id, "error", err)
		c.removeInstance(inst.Id)
		return
	}
	if !healthcheck.Enabled(policy) || !healthCheckControllerEligibleState(inst.State) {
		c.removeInstance(inst.Id)
		return
	}

	interval, _, _, err := healthcheck.DurationConfig(policy)
	if err != nil {
		c.log.Warn("invalid health check duration", "instance_id", inst.Id, "error", err)
		c.removeInstance(inst.Id)
		return
	}

	runtime := healthcheck.CloneRuntime(inst.HealthCheckRuntime)
	if resetRuntime {
		runtime = nil
	}

	delay := interval
	if immediate || runtime == nil || runtime.LastCheckedAt == nil {
		delay = 0
	}

	target := toHealthCheckInstance(inst)
	target.Runtime = runtime

	c.mu.Lock()
	state, ok := c.states[inst.Id]
	if !ok {
		state = &healthCheckControllerState{}
		c.states[inst.Id] = state
	}
	state.instance = target
	state.policy = policy
	c.scheduleLocked(inst.Id, state, delay)
	c.mu.Unlock()
}

func healthCheckControllerEligibleState(state State) bool {
	return state == StateInitializing || state == StateRunning
}

func (c *HealthCheckController) removeInstance(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if state := c.states[id]; state != nil && state.timer != nil {
		state.timer.Stop()
	}
	delete(c.states, id)
}

func (c *HealthCheckController) runCheck(ctx context.Context, id string) {
	c.mu.Lock()
	state := c.states[id]
	if state == nil {
		c.mu.Unlock()
		return
	}
	inst := state.instance
	policy := healthcheck.ClonePolicy(state.policy)
	previous := healthcheck.CloneRuntime(inst.Runtime)
	c.mu.Unlock()

	_, timeout, _, err := healthcheck.DurationConfig(policy)
	if err != nil {
		c.log.Warn("invalid health check duration", "instance_id", id, "error", err)
		c.removeInstance(id)
		return
	}

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	result := c.runner.Check(checkCtx, inst, policy)
	cancel()

	runtime := healthcheck.ApplyProbeResult(policy, inst, previous, c.now(), result)
	if err := c.store.SetHealthCheckRuntime(ctx, id, runtime); err != nil {
		c.log.Warn("failed to persist health check status", "instance_id", id, "error", err)
	}

	interval, _, _, err := healthcheck.DurationConfig(policy)
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

func (c *HealthCheckController) scheduleLocked(id string, state *healthCheckControllerState, delay time.Duration) {
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

func (c *HealthCheckController) stopAllTimers() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, state := range c.states {
		if state.timer != nil {
			state.timer.Stop()
		}
	}
	c.states = make(map[string]*healthCheckControllerState)
}

type healthCheckExecRunner struct {
	manager Manager
}

func (r healthCheckExecRunner) Run(ctx context.Context, inst healthcheck.Instance, check healthcheck.ExecCheck, timeout time.Duration) error {
	dialer, err := r.manager.GetVsockDialer(ctx, inst.ID)
	if err != nil {
		return err
	}

	timeoutSeconds := int32((timeout + time.Second - 1) / time.Second)
	if timeoutSeconds < 1 {
		timeoutSeconds = 1
	}
	exit, err := guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command: check.Command,
		Cwd:     check.WorkingDir,
		Timeout: timeoutSeconds,
	})
	if err != nil {
		return err
	}
	if exit == nil {
		return fmt.Errorf("exec health check exited without status")
	}
	if exit.Code != 0 {
		return fmt.Errorf("exec health check exited with status %d", exit.Code)
	}
	return nil
}

func toHealthCheckInstance(inst *Instance) healthcheck.Instance {
	if inst == nil {
		return healthcheck.Instance{}
	}
	return healthcheck.Instance{
		ID:              inst.Id,
		Name:            inst.Name,
		State:           string(inst.State),
		NetworkEnabled:  inst.NetworkEnabled,
		IP:              inst.IP,
		StartedAt:       inst.StartedAt,
		GuestAgentReady: inst.GuestAgentReadyAt != nil,
		SkipGuestAgent:  inst.SkipGuestAgent,
		HealthCheck:     inst.HealthCheck,
		Runtime:         healthcheck.CloneRuntime(inst.HealthCheckRuntime),
	}
}
