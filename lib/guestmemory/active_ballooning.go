package guestmemory

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

var (
	ErrActiveBallooningDisabled = errors.New("active ballooning is disabled")
	ErrGuestMemoryDisabled      = errors.New("guest memory reclaim is disabled")
)

// ActiveBallooningConfig controls host-driven balloon reclaim behavior.
type ActiveBallooningConfig struct {
	Enabled bool

	PollInterval time.Duration

	PressureHighWatermarkAvailablePercent int
	PressureLowWatermarkAvailablePercent  int

	ProtectedFloorPercent  int
	ProtectedFloorMinBytes int64

	MinAdjustmentBytes int64
	PerVMMaxStepBytes  int64
	PerVMCooldown      time.Duration
}

// DefaultActiveBallooningConfig returns conservative defaults for active reclaim.
func DefaultActiveBallooningConfig() ActiveBallooningConfig {
	return ActiveBallooningConfig{
		Enabled:                               false,
		PollInterval:                          2 * time.Second,
		PressureHighWatermarkAvailablePercent: 10,
		PressureLowWatermarkAvailablePercent:  15,
		ProtectedFloorPercent:                 50,
		ProtectedFloorMinBytes:                512 * 1024 * 1024,
		MinAdjustmentBytes:                    64 * 1024 * 1024,
		PerVMMaxStepBytes:                     256 * 1024 * 1024,
		PerVMCooldown:                         5 * time.Second,
	}
}

// Normalize applies defaults and clamps invalid values.
func (c ActiveBallooningConfig) Normalize() ActiveBallooningConfig {
	d := DefaultActiveBallooningConfig()

	if c.PollInterval <= 0 {
		c.PollInterval = d.PollInterval
	}
	if c.PressureHighWatermarkAvailablePercent <= 0 || c.PressureHighWatermarkAvailablePercent >= 100 {
		c.PressureHighWatermarkAvailablePercent = d.PressureHighWatermarkAvailablePercent
	}
	if c.PressureLowWatermarkAvailablePercent <= 0 || c.PressureLowWatermarkAvailablePercent >= 100 {
		c.PressureLowWatermarkAvailablePercent = d.PressureLowWatermarkAvailablePercent
	}
	if c.PressureLowWatermarkAvailablePercent <= c.PressureHighWatermarkAvailablePercent {
		c.PressureLowWatermarkAvailablePercent = c.PressureHighWatermarkAvailablePercent + 1
		if c.PressureLowWatermarkAvailablePercent >= 100 {
			c.PressureHighWatermarkAvailablePercent = d.PressureHighWatermarkAvailablePercent
			c.PressureLowWatermarkAvailablePercent = d.PressureLowWatermarkAvailablePercent
		}
	}
	if c.ProtectedFloorPercent <= 0 || c.ProtectedFloorPercent >= 100 {
		c.ProtectedFloorPercent = d.ProtectedFloorPercent
	}
	if c.ProtectedFloorMinBytes <= 0 {
		c.ProtectedFloorMinBytes = d.ProtectedFloorMinBytes
	}
	if c.MinAdjustmentBytes <= 0 {
		c.MinAdjustmentBytes = d.MinAdjustmentBytes
	}
	if c.PerVMMaxStepBytes <= 0 {
		c.PerVMMaxStepBytes = d.PerVMMaxStepBytes
	}
	if c.PerVMCooldown <= 0 {
		c.PerVMCooldown = d.PerVMCooldown
	}

	return c
}

// BalloonVM describes a running VM that may participate in reclaim.
type BalloonVM struct {
	ID                  string
	Name                string
	HypervisorType      hypervisor.Type
	SocketPath          string
	AssignedMemoryBytes int64
}

// Source lists reclaim-eligible VMs.
type Source interface {
	ListBalloonVMs(ctx context.Context) ([]BalloonVM, error)
}

// Controller coordinates automatic and manual reclaim.
type Controller interface {
	Start(ctx context.Context) error
	TriggerReclaim(ctx context.Context, req ManualReclaimRequest) (ManualReclaimResponse, error)
}

// ManualReclaimRequest triggers a proactive reclaim cycle.
type ManualReclaimRequest struct {
	ReclaimBytes int64
	HoldFor      time.Duration
	DryRun       bool
	Reason       string
}

// HostPressureState summarizes host memory pressure.
type HostPressureState string

const (
	HostPressureStateHealthy  HostPressureState = "healthy"
	HostPressureStatePressure HostPressureState = "pressure"
)

// ManualReclaimAction captures one VM's reclaim plan/result.
type ManualReclaimAction struct {
	InstanceID                     string
	InstanceName                   string
	Hypervisor                     hypervisor.Type
	AssignedMemoryBytes            int64
	ProtectedFloorBytes            int64
	PreviousTargetGuestMemoryBytes int64
	PlannedTargetGuestMemoryBytes  int64
	TargetGuestMemoryBytes         int64
	AppliedReclaimBytes            int64
	Status                         string
	Error                          string
}

// ManualReclaimResponse summarizes the last reconcile result.
type ManualReclaimResponse struct {
	RequestedReclaimBytes int64
	PlannedReclaimBytes   int64
	AppliedReclaimBytes   int64
	HoldUntil             *time.Time
	HostAvailableBytes    int64
	HostPressureState     HostPressureState
	Actions               []ManualReclaimAction
}

// HostPressureSample captures the host memory snapshot used for reclaim decisions.
type HostPressureSample struct {
	TotalBytes       int64
	AvailableBytes   int64
	AvailablePercent float64
	Stressed         bool
}

// PressureSampler provides host memory pressure samples.
type PressureSampler interface {
	Sample(ctx context.Context) (HostPressureSample, error)
}

type controller struct {
	policy  Policy
	config  ActiveBallooningConfig
	source  Source
	sampler PressureSampler
	log     *slog.Logger
	metrics *Metrics
	tracer  trace.Tracer

	reconcileMu syncState
}

type syncState struct {
	mu            chan struct{}
	pressureState HostPressureState
	manualHold    *manualHold
	lastApplied   map[string]time.Time
	newClient     func(hypervisor.Type, string) (hypervisor.Hypervisor, error)
}

type manualHold struct {
	reclaimBytes int64
	until        time.Time
}

// NewController creates the active ballooning controller.
func NewController(policy Policy, cfg ActiveBallooningConfig, source Source, log *slog.Logger) Controller {
	return NewControllerWithSampler(policy, cfg, source, newHostPressureSampler(), log)
}

// NewControllerWithSampler creates the active ballooning controller with an injected
// host pressure sampler. This is primarily useful for tests that need deterministic
// reclaim behavior.
func NewControllerWithSampler(policy Policy, cfg ActiveBallooningConfig, source Source, sampler PressureSampler, log *slog.Logger) Controller {
	if log == nil {
		log = slog.Default()
	}
	if sampler == nil {
		sampler = newHostPressureSampler()
	}

	metrics, err := NewMetrics(otel.GetMeterProvider().Meter("hypeman"))
	if err != nil {
		log.Warn("failed to initialize guest memory metrics", "error", err)
	}

	c := &controller{
		policy:  policy.Normalize(),
		config:  cfg.Normalize(),
		source:  source,
		sampler: sampler,
		log:     log,
		metrics: metrics,
		tracer:  otel.Tracer("hypeman/guestmemory"),
		reconcileMu: syncState{
			mu:            make(chan struct{}, 1),
			pressureState: HostPressureStateHealthy,
			lastApplied:   make(map[string]time.Time),
			newClient:     hypervisor.NewClient,
		},
	}
	c.reconcileMu.mu <- struct{}{}
	return c
}
