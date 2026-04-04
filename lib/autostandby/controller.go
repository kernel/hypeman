package autostandby

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const defaultPollInterval = 5 * time.Second

// InstanceStore supplies the controller with instance state and standby actions.
type InstanceStore interface {
	ListInstances(ctx context.Context) ([]Instance, error)
	StandbyInstance(ctx context.Context, id string) error
}

// ConnectionSource lists current TCP flows that may keep an instance awake.
type ConnectionSource interface {
	ListConnections(ctx context.Context) ([]Connection, error)
}

// Controller decides when eligible instances should transition to standby.
type Controller struct {
	store        InstanceStore
	source       ConnectionSource
	log          *slog.Logger
	now          func() time.Time
	pollInterval time.Duration
	idleSince    map[string]time.Time
}

// NewController creates a new auto-standby controller.
func NewController(store InstanceStore, source ConnectionSource, log *slog.Logger, pollInterval time.Duration) *Controller {
	if log == nil {
		log = slog.Default()
	}
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	return &Controller{
		store:        store,
		source:       source,
		log:          log,
		now:          time.Now,
		pollInterval: pollInterval,
		idleSince:    make(map[string]time.Time),
	}
}

// Run starts the controller loop and blocks until the context is canceled.
func (c *Controller) Run(ctx context.Context) error {
	c.log.Info("auto-standby controller started", "poll_interval", c.pollInterval)
	if err := c.Poll(ctx); err != nil {
		c.log.Warn("auto-standby poll failed", "error", err)
	}

	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := c.Poll(ctx); err != nil {
				c.log.Warn("auto-standby poll failed", "error", err)
			}
		}
	}
}

// Poll runs a single reconciliation pass.
func (c *Controller) Poll(ctx context.Context) error {
	instances, err := c.store.ListInstances(ctx)
	if err != nil {
		return err
	}
	conns, err := c.source.ListConnections(ctx)
	if err != nil {
		return err
	}

	now := c.now().UTC()
	seen := make(map[string]struct{}, len(instances))
	var reconcileErrs []error

	for _, inst := range instances {
		seen[inst.ID] = struct{}{}

		if !eligible(inst) {
			delete(c.idleSince, inst.ID)
			continue
		}

		count, idleTimeout, err := ActiveInboundCount(inst, conns)
		if err != nil {
			delete(c.idleSince, inst.ID)
			reconcileErrs = append(reconcileErrs, err)
			continue
		}

		if count > 0 {
			delete(c.idleSince, inst.ID)
			continue
		}

		start, ok := c.idleSince[inst.ID]
		if !ok {
			c.idleSince[inst.ID] = now
			continue
		}
		if now.Sub(start) < idleTimeout {
			continue
		}

		if err := c.store.StandbyInstance(ctx, inst.ID); err != nil {
			c.log.Warn("auto-standby standby attempt failed", "instance_id", inst.ID, "instance_name", inst.Name, "error", err)
		} else {
			c.log.Info("instance entered standby due to inbound inactivity", "instance_id", inst.ID, "instance_name", inst.Name, "idle_timeout", idleTimeout)
		}

		// Reset the timer after every attempt to avoid retrying every poll interval
		// when the standby transition fails for a transient reason.
		c.idleSince[inst.ID] = now
	}

	for id := range c.idleSince {
		if _, ok := seen[id]; !ok {
			delete(c.idleSince, id)
		}
	}

	return errors.Join(reconcileErrs...)
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
