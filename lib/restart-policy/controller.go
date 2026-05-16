package restartpolicy

import (
	"context"
	"log/slog"
	"time"
)

const (
	StateInitializing = "Initializing"
	StateRunning      = "Running"
	StateStopped      = "Stopped"

	DefaultReconcileInterval = 5 * time.Second
)

type InstanceEventAction string

const (
	InstanceEventCreate InstanceEventAction = "create"
	InstanceEventUpdate InstanceEventAction = "update"
	InstanceEventStart  InstanceEventAction = "start"
	InstanceEventStop   InstanceEventAction = "stop"
	InstanceEventDelete InstanceEventAction = "delete"
)

type InstanceEvent struct {
	Action     InstanceEventAction
	InstanceID string
	Instance   *Instance
}

type Instance struct {
	ID            string
	State         string
	StartedAt     *time.Time
	ExitCode      *int
	RestartPolicy *Policy
	RestartStatus Status
}

type Store interface {
	ListInstances(ctx context.Context) ([]Instance, error)
	RestartInstance(ctx context.Context, id string) error
	SetRestartStatus(ctx context.Context, id string, status Status) error
	SubscribeInstanceEvents() (<-chan InstanceEvent, func(), error)
}

type ControllerOptions struct {
	Log               *slog.Logger
	Now               func() time.Time
	ReconcileInterval time.Duration
}

type Controller struct {
	store             Store
	log               *slog.Logger
	now               func() time.Time
	reconcileInterval time.Duration
}

func NewController(store Store, opts ControllerOptions) *Controller {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	interval := opts.ReconcileInterval
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}
	return &Controller{
		store:             store,
		log:               log,
		now:               now,
		reconcileInterval: interval,
	}
}

func (c *Controller) Run(ctx context.Context) error {
	c.log.Info("restart policy controller started", "reconcile_interval", c.reconcileInterval)
	if err := c.Reconcile(ctx); err != nil {
		c.log.Warn("restart policy startup reconcile failed", "error", err)
	}

	events, unsubscribe, err := c.store.SubscribeInstanceEvents()
	if err != nil {
		return err
	}
	defer unsubscribe()

	ticker := time.NewTicker(c.reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-events:
			if !ok {
				return nil
			}
			if event.Action == InstanceEventDelete {
				continue
			}
			if event.Instance == nil {
				if err := c.Reconcile(ctx); err != nil {
					c.log.Warn("restart policy event reconcile failed", "instance_id", event.InstanceID, "error", err)
				}
				continue
			}
			if err := c.reconcileInstance(ctx, *event.Instance); err != nil {
				c.log.Warn("restart policy event handling failed", "instance_id", event.InstanceID, "error", err)
			}
		case <-ticker.C:
			if err := c.Reconcile(ctx); err != nil {
				c.log.Warn("restart policy reconcile failed", "error", err)
			}
		}
	}
}

func (c *Controller) Reconcile(ctx context.Context) error {
	instances, err := c.store.ListInstances(ctx)
	if err != nil {
		return err
	}
	for _, inst := range instances {
		if err := c.reconcileInstance(ctx, inst); err != nil {
			c.log.Warn("restart policy reconcile failed for instance", "instance_id", inst.ID, "error", err)
		}
	}
	return nil
}

func (c *Controller) reconcileInstance(ctx context.Context, inst Instance) error {
	policy, err := NormalizePolicy(inst.RestartPolicy)
	if err != nil {
		return err
	}
	if policy == nil {
		return nil
	}

	status := inst.RestartStatus
	now := c.now().UTC()

	if shouldResetStableAttempts(policy, status, inst, now) {
		c.log.Info("restart policy stable window reached", "instance_id", inst.ID, "attempts", status.Attempts)
		return c.store.SetRestartStatus(ctx, inst.ID, Status{})
	}

	if inst.State != StateStopped {
		return nil
	}
	if status.BlockedReason != "" {
		return nil
	}
	if !ShouldRestartInstance(policy, inst.ExitCode, status) {
		return nil
	}

	nextStatus, shouldAttempt := PrepareAttempt(policy, status, now)
	if !shouldAttempt {
		if !EqualStatus(status, nextStatus) {
			return c.store.SetRestartStatus(ctx, inst.ID, nextStatus)
		}
		return nil
	}

	reason := nextStatus.LastReason
	nextStatus.LastReason = ""
	if err := c.store.SetRestartStatus(ctx, inst.ID, nextStatus); err != nil {
		return err
	}

	c.log.Info("restart policy starting instance", "instance_id", inst.ID, "attempt", nextStatus.Attempts)
	if err := c.store.RestartInstance(ctx, inst.ID); err != nil {
		nextStatus = AfterFailedAttempt(policy, nextStatus, now)
		nextStatus.LastReason = reason
		if statusErr := c.store.SetRestartStatus(ctx, inst.ID, nextStatus); statusErr != nil {
			c.log.Warn("failed to persist restart status after restart failure", "instance_id", inst.ID, "error", statusErr)
		}
		return err
	}
	return nil
}

func shouldResetStableAttempts(policy *Policy, status Status, inst Instance, now time.Time) bool {
	if status.Attempts == 0 || inst.StartedAt == nil {
		return false
	}
	if inst.State != StateRunning && inst.State != StateInitializing {
		return false
	}
	return !now.Before(inst.StartedAt.UTC().Add(StableAfter(policy)))
}
