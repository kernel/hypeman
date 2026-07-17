package uffdgraduate

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"
)

// ControllerOptions configures logging, metrics, and time.
type ControllerOptions struct {
	Log   *slog.Logger
	Meter metric.Meter
	Now   func() time.Time
}

// failureBackoff is how long a failed graduation is left alone before it is
// retried. Every attempt reads the whole memory image, so retrying on each
// scan would be expensive.
const failureBackoff = 5 * time.Minute

// Controller periodically detaches running VMs that have soaked past
// MinSessionAge from their snapshot memory pager, so long-lived VMs stop
// depending on a pager and old pager versions drain to zero and exit.
type Controller struct {
	store InstanceStore
	cfg   Config
	log   *slog.Logger
	now   func() time.Time

	metrics *Metrics
	wg      sync.WaitGroup

	mu          sync.Mutex
	firstSeen   map[string]time.Time
	lastFailure map[string]time.Time
	inFlight    map[string]struct{}
}

// NewController builds a controller. cfg is normalized here.
func NewController(store InstanceStore, cfg Config, opts ControllerOptions) *Controller {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	c := &Controller{
		store:       store,
		cfg:         cfg.Normalize(),
		log:         log,
		now:         now,
		firstSeen:   make(map[string]time.Time),
		lastFailure: make(map[string]time.Time),
		inFlight:    make(map[string]struct{}),
	}
	c.metrics = newMetrics(opts.Meter, c)
	return c
}

// Run scans on an interval until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	if !c.cfg.Enabled {
		<-ctx.Done()
		return nil
	}
	c.log.Info("uffd graduation controller started",
		"min_session_age", c.cfg.MinSessionAge,
		"max_concurrent", c.cfg.MaxConcurrent,
		"scan_interval", c.cfg.ScanInterval,
	)

	ticker := time.NewTicker(c.cfg.ScanInterval)
	defer ticker.Stop()

	c.scan(ctx)
	for {
		select {
		case <-ctx.Done():
			c.wg.Wait()
			return nil
		case <-ticker.C:
			c.scan(ctx)
		}
	}
}

func (c *Controller) scan(ctx context.Context) {
	insts, err := c.store.ListPagerInstances(ctx)
	if err != nil {
		c.recordError("list")
		c.log.Warn("uffd graduation scan failed to list instances", "error", err)
		return
	}
	target := c.store.TargetVersion()
	now := c.now()

	c.mu.Lock()
	present := make(map[string]struct{}, len(insts))
	for _, inst := range insts {
		present[inst.ID] = struct{}{}
		if _, ok := c.firstSeen[inst.ID]; !ok {
			c.firstSeen[inst.ID] = now
		}
	}
	for id := range c.firstSeen {
		if _, ok := present[id]; !ok {
			delete(c.firstSeen, id)
		}
	}
	for id := range c.lastFailure {
		if _, ok := present[id]; !ok {
			delete(c.lastFailure, id)
		}
	}

	candidates := c.selectCandidatesLocked(insts, target, now)
	launch := make([]Instance, 0, len(candidates))
	for _, inst := range candidates {
		if len(c.inFlight) >= c.cfg.MaxConcurrent {
			break
		}
		if _, busy := c.inFlight[inst.ID]; busy {
			continue
		}
		c.inFlight[inst.ID] = struct{}{}
		launch = append(launch, inst)
	}
	c.mu.Unlock()

	for _, inst := range launch {
		c.wg.Add(1)
		go c.graduate(ctx, inst)
	}
}

// selectCandidatesLocked returns the soaked instances that should graduate,
// ordered by priority: outdated pager versions first, then oldest first.
// Recently failed instances are left alone until their backoff elapses.
func (c *Controller) selectCandidatesLocked(insts []Instance, target string, now time.Time) []Instance {
	type candidate struct {
		inst     Instance
		outdated bool
		age      time.Duration
	}

	soaked := make([]candidate, 0, len(insts))
	for _, inst := range insts {
		seen, ok := c.firstSeen[inst.ID]
		if !ok {
			continue
		}
		if now.Sub(seen) < c.cfg.MinSessionAge {
			continue
		}
		if _, busy := c.inFlight[inst.ID]; busy {
			continue
		}
		if failed, ok := c.lastFailure[inst.ID]; ok && now.Sub(failed) < failureBackoff {
			continue
		}
		outdated := target != "" && inst.PagerVersion != "" && inst.PagerVersion != target
		soaked = append(soaked, candidate{inst: inst, outdated: outdated, age: now.Sub(seen)})
	}

	sort.SliceStable(soaked, func(i, j int) bool {
		if soaked[i].outdated != soaked[j].outdated {
			return soaked[i].outdated
		}
		return soaked[i].age > soaked[j].age
	})

	out := make([]Instance, 0, len(soaked))
	for _, s := range soaked {
		out = append(out, s.inst)
	}
	return out
}

func (c *Controller) graduate(ctx context.Context, inst Instance) {
	defer c.wg.Done()
	defer func() {
		c.mu.Lock()
		delete(c.inFlight, inst.ID)
		c.mu.Unlock()
	}()

	gctx, cancel := context.WithTimeout(ctx, c.cfg.CompletionTimeout)
	defer cancel()

	c.log.Info("graduating instance off uffd pager",
		"instance_id", inst.ID, "instance_name", inst.Name, "pager_version", inst.PagerVersion)

	if err := c.store.GraduateInstance(gctx, inst.ID); err != nil {
		c.recordAttempt("error")
		c.recordError("graduate")
		c.mu.Lock()
		c.lastFailure[inst.ID] = c.now()
		c.mu.Unlock()
		c.log.Warn("uffd graduation failed, backing off",
			"instance_id", inst.ID, "instance_name", inst.Name, "backoff", failureBackoff, "error", err)
		return
	}

	c.recordAttempt("success")
	// Drop first-seen so a later rebind (e.g. after a future restore) restarts
	// its soak rather than graduating immediately.
	c.mu.Lock()
	delete(c.firstSeen, inst.ID)
	delete(c.lastFailure, inst.ID)
	c.mu.Unlock()
	c.log.Info("instance graduated off uffd pager", "instance_id", inst.ID, "instance_name", inst.Name)
}

func (c *Controller) trackedSessions() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.firstSeen)
}

func (c *Controller) inFlightCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.inFlight)
}
