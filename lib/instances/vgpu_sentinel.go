package instances

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/logger"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	vgpuSentinelPollInterval       = 5 * time.Second
	vgpuSentinelPollTimeout        = 5 * time.Second
	vgpuSentinelMaxConcurrentPolls = 64
)

type vgpuSentinelTarget struct {
	instanceID string
	vfAddress  string
	assignedAt string
}

type vgpuSentinelStore interface {
	listVGPUSentinelTargets(ctx context.Context) ([]vgpuSentinelTarget, error)
	getVGPUSentinelTarget(ctx context.Context, instanceID string) (vgpuSentinelTarget, bool, error)
}

var _ vgpuSentinelStore = (*manager)(nil)

// VGPUSentinelController quarantines VFs whose guest reports a wedged driver
// init. It polls each vendor-VFIO instance's guest agent over vsock; the
// serial console is shared with workload output, so nothing read from logs is
// trusted. The health store deduplicates repeated reports per assignment, so
// polling is idempotent and a failed persist retries on the next tick.
type VGPUSentinelController struct {
	store              vgpuSentinelStore
	log                *slog.Logger
	interval           time.Duration
	reportFailure      func(devices.VFInitFailureReport) (devices.VFReportResult, error)
	reportSuccess      func(devices.VFInitSuccessReport) (devices.VFSuccessResult, error)
	guestGPUInitStatus func(ctx context.Context, instanceID string) (guest.GPUInitState, string, error)
	initFailures       metric.Int64Counter
	quarantines        metric.Int64Counter
	checks             metric.Int64Counter
	discoverFramework  func() (devices.VGPUFramework, []devices.VirtualFunction, error)
	probeErrLoggedAt   time.Time
}

func NewVGPUSentinelController(manager Manager, meter metric.Meter, log *slog.Logger) (*VGPUSentinelController, error) {
	if manager == nil {
		return nil, fmt.Errorf("instance manager is nil")
	}
	if log == nil {
		return nil, fmt.Errorf("logger is nil")
	}
	store, ok := manager.(vgpuSentinelStore)
	if !ok {
		return nil, fmt.Errorf("instance manager %T does not implement vgpuSentinelStore", manager)
	}

	initFailures, err := meter.Int64Counter(
		"hypeman_instances_vgpu_sentinel_init_failures_total",
		metric.WithDescription("Total guest-reported vGPU driver init failures recorded by the sentinel (one per instance assignment)"),
	)
	if err != nil {
		return nil, err
	}
	quarantines, err := meter.Int64Counter(
		"hypeman_instances_vgpu_sentinel_quarantines_total",
		metric.WithDescription("Total VFs quarantined by the vGPU sentinel"),
	)
	if err != nil {
		return nil, err
	}
	checks, err := meter.Int64Counter(
		"hypeman_instances_vgpu_sentinel_checks_total",
		metric.WithDescription("Total vGPU sentinel checks by result"),
	)
	if err != nil {
		return nil, err
	}
	_, err = meter.Int64ObservableGauge(
		"hypeman_instances_vgpu_quarantined_vfs",
		metric.WithDescription("Number of vGPU virtual functions currently quarantined"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(devices.TotalQuarantinedVFs()))
			return nil
		}),
	)
	if err != nil {
		return nil, err
	}
	_, err = meter.Int64ObservableGauge(
		"hypeman_instances_vgpu_vf_health_store_unavailable",
		metric.WithDescription("1 when the persisted VF health state failed to load or persist; quarantine mutations are refused and vGPU placement is disabled until it is repaired"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			if devices.VFHealthStoreUnavailable() {
				o.Observe(1)
			} else {
				o.Observe(0)
			}
			return nil
		}),
	)
	if err != nil {
		return nil, err
	}

	return &VGPUSentinelController{
		store:         store,
		log:           log.With("controller", "vgpu_sentinel"),
		interval:      vgpuSentinelPollInterval,
		reportFailure: devices.ReportVFInitFailure,
		reportSuccess: devices.ReportVFInitSuccess,
		guestGPUInitStatus: func(ctx context.Context, instanceID string) (guest.GPUInitState, string, error) {
			dialer, err := manager.GetVsockDialer(ctx, instanceID)
			if err != nil {
				return guest.GPUInitState_GPU_INIT_STATE_UNKNOWN, "", err
			}
			return guest.GetGPUInitStatus(ctx, dialer)
		},
		discoverFramework: devices.DiscoverVGPU,
		initFailures:      initFailures,
		quarantines:       quarantines,
		checks:            checks,
	}, nil
}

func (c *VGPUSentinelController) Run(ctx context.Context) error {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	vendorVFIO := false
	for {
		if !vendorVFIO {
			var err error
			vendorVFIO, err = c.probeVendorVFIO()
			if err != nil && time.Since(c.probeErrLoggedAt) >= time.Minute {
				c.log.Warn("vGPU sentinel framework discovery failed; retrying", "error", err)
				c.probeErrLoggedAt = time.Now()
			}
			if err == nil && !vendorVFIO {
				return nil
			}
			if vendorVFIO {
				c.log.Info("vGPU sentinel controller started")
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.pollOnce(ctx)
		}
	}
}

func (c *VGPUSentinelController) probeVendorVFIO() (bool, error) {
	framework, _, err := c.discoverFramework()
	if err != nil {
		return false, err
	}
	if framework == devices.VGPUFrameworkNone {
		c.log.Info("vGPU sentinel controller exiting: host has no vGPU framework")
		return false, nil
	}
	if framework != devices.VGPUFrameworkVendorVFIO {
		c.log.Info("vGPU sentinel controller exiting: host vGPU framework is not vendor VFIO", "framework", string(framework))
		return false, nil
	}
	return true, nil
}

func (c *VGPUSentinelController) pollOnce(ctx context.Context) {
	targets, err := c.store.listVGPUSentinelTargets(ctx)
	if err != nil {
		c.recordCheck(ctx, "list_error")
		c.log.WarnContext(ctx, "vGPU sentinel failed to list instances", "error", err)
		return
	}
	var group errgroup.Group
	group.SetLimit(vgpuSentinelMaxConcurrentPolls)
	for _, target := range targets {
		group.Go(func() error {
			c.pollTarget(ctx, target)
			return nil
		})
	}
	_ = group.Wait()
}

func (c *VGPUSentinelController) pollTarget(ctx context.Context, target vgpuSentinelTarget) {
	pollCtx, cancel := context.WithTimeout(ctx, vgpuSentinelPollTimeout)
	state, nvrm, err := c.guestGPUInitStatus(pollCtx, target.instanceID)
	cancel()
	if err != nil {
		result := "rpc_error"
		if status.Code(err) == codes.Unimplemented {
			result = "unsupported_agent"
		}
		c.recordCheck(ctx, result)
		// The agent can be unreachable while the instance stops or boots; the
		// next tick polls again.
		c.log.DebugContext(ctx, "vGPU sentinel cannot query the guest agent",
			"instance_id", target.instanceID, "error", err)
		return
	}
	switch state {
	case guest.GPUInitState_GPU_INIT_STATE_FAILED:
		c.recordCheck(ctx, "failed")
		c.handleFailure(ctx, target, nvrm)
	case guest.GPUInitState_GPU_INIT_STATE_OK:
		c.recordCheck(ctx, "ok")
		c.handleSuccess(ctx, target)
	default:
		c.recordCheck(ctx, "unknown")
	}
}

func (c *VGPUSentinelController) recordCheck(ctx context.Context, result string) {
	c.checks.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

// confirmAssignment rejects a report only when the instance now holds a
// different VF assignment. Released assignments remain attributable to the
// assignment captured in the poll target.
func (c *VGPUSentinelController) confirmAssignment(ctx context.Context, target vgpuSentinelTarget) (bool, error) {
	current, ok, err := c.store.getVGPUSentinelTarget(ctx, target.instanceID)
	if err != nil {
		return false, err
	}
	return !ok || (current.vfAddress == target.vfAddress && current.assignedAt == target.assignedAt), nil
}

func (c *VGPUSentinelController) handleFailure(ctx context.Context, target vgpuSentinelTarget, nvrm string) {
	unchanged, err := c.confirmAssignment(ctx, target)
	if err != nil {
		c.log.WarnContext(ctx, "vGPU sentinel could not confirm assignment before recording an init failure",
			"vf", target.vfAddress, "instance_id", target.instanceID, "error", err)
		return
	}
	if !unchanged {
		c.log.InfoContext(ctx, "vGPU sentinel skipping init failure: assignment changed during poll",
			"vf", target.vfAddress, "instance_id", target.instanceID)
		return
	}
	result, err := c.reportFailure(devices.VFInitFailureReport{
		VFAddress:  target.vfAddress,
		InstanceID: target.instanceID,
		AssignedAt: target.assignedAt,
	})
	if err != nil {
		c.log.ErrorContext(ctx, "failed to record vGPU VF init failure",
			"vf", target.vfAddress, "instance_id", target.instanceID, "error", err)
		return
	}
	switch result.Outcome {
	case devices.VFReportQuarantined:
		c.log.ErrorContext(ctx, "quarantined wedged vGPU VF",
			"vf", target.vfAddress,
			"instance_id", target.instanceID,
			"nvrm", nvrm,
			"failures", result.Failures,
			"threshold", result.Threshold,
		)
		c.initFailures.Add(ctx, 1)
		c.quarantines.Add(ctx, 1)
	case devices.VFReportRecorded:
		c.log.WarnContext(ctx, "recorded vGPU VF init failure below quarantine threshold",
			"vf", target.vfAddress,
			"instance_id", target.instanceID,
			"nvrm", nvrm,
			"failures", result.Failures,
			"threshold", result.Threshold,
		)
		c.initFailures.Add(ctx, 1)
	}
}

func (c *VGPUSentinelController) handleSuccess(ctx context.Context, target vgpuSentinelTarget) {
	unchanged, err := c.confirmAssignment(ctx, target)
	if err != nil {
		c.log.WarnContext(ctx, "vGPU sentinel could not confirm assignment before clearing init failures",
			"vf", target.vfAddress, "instance_id", target.instanceID, "error", err)
		return
	}
	if !unchanged {
		return
	}
	result, err := c.reportSuccess(devices.VFInitSuccessReport{
		VFAddress:  target.vfAddress,
		InstanceID: target.instanceID,
		AssignedAt: target.assignedAt,
	})
	if err != nil {
		// The guest keeps reporting OK, so the next poll retries the clear.
		c.log.WarnContext(ctx, "vGPU sentinel failed to clear recorded init failures; will retry",
			"vf", target.vfAddress, "instance_id", target.instanceID, "error", err)
		return
	}
	if result.Rescinded {
		c.log.InfoContext(ctx, "rescinded vGPU VF quarantine after successful driver init from the triggering assignment",
			"vf", target.vfAddress, "instance_id", target.instanceID, "cleared", result.Cleared)
	} else if result.Cleared > 0 {
		c.log.InfoContext(ctx, "cleared recorded vGPU VF init failures after successful driver init",
			"vf", target.vfAddress, "instance_id", target.instanceID, "cleared", result.Cleared)
	}
}

func (m *manager) listVGPUSentinelTargets(ctx context.Context) ([]vgpuSentinelTarget, error) {
	files, statErr, err := m.walkMetadataFiles()
	if err != nil {
		return nil, err
	}
	if statErr != nil {
		logger.FromContext(ctx).WarnContext(ctx, "vGPU sentinel cannot stat some instance metadata; their VFs are not scanned", "error", statErr)
	}
	targets := make([]vgpuSentinelTarget, 0, len(files))
	for _, file := range files {
		id := filepath.Base(filepath.Dir(file))
		target, ok, err := m.getVGPUSentinelTarget(ctx, id)
		if err != nil {
			logger.FromContext(ctx).WarnContext(ctx, "vGPU sentinel skipping unreadable instance metadata", "instance_id", id, "error", err)
			continue
		}
		if ok {
			targets = append(targets, target)
		}
	}
	return targets, nil
}

func (m *manager) getVGPUSentinelTarget(_ context.Context, instanceID string) (vgpuSentinelTarget, bool, error) {
	meta, err := m.loadMetadata(instanceID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return vgpuSentinelTarget{}, false, nil
		}
		return vgpuSentinelTarget{}, false, err
	}
	if meta.GPURetainedForCleanup || meta.GPUFramework != devices.VGPUFrameworkVendorVFIO || meta.GPUDevicePath == "" {
		return vgpuSentinelTarget{}, false, nil
	}
	assignedAt := ""
	if meta.GPUAssignedAt != nil {
		assignedAt = meta.GPUAssignedAt.UTC().Format(time.RFC3339Nano)
	}
	return vgpuSentinelTarget{
		instanceID: instanceID,
		vfAddress:  filepath.Base(meta.GPUDevicePath),
		assignedAt: assignedAt,
	}, true, nil
}
