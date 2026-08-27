package instances

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/logger"
	"go.opentelemetry.io/otel/metric"
)

const (
	vgpuSentinelScanInterval       = 5 * time.Second
	vgpuSentinelMaxLineBytes       = 64 * 1024
	vgpuSentinelCorroborateTimeout = 5 * time.Second
)

// Require a standalone guest-agent line so echoed marker text cannot count against a VF.
var (
	vgpuSentinelFailedPattern = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} \[guest-agent\] (HYPEMAN-GPU-INIT-FAILED ts=\S+ nvrm="NVRM: [^"\r\n]*RmInitAdapter failed![^"\r\n]*")\r?\n?$`)
	vgpuSentinelOKPattern     = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} \[guest-agent\] (HYPEMAN-GPU-INIT-OK ts=\S+)\r?\n?$`)
)

// VGPUSentinelEvent classifies a guest-agent vGPU marker line.
type VGPUSentinelEvent int

const (
	VGPUSentinelEventNone VGPUSentinelEvent = iota
	VGPUSentinelEventInitFailed
	VGPUSentinelEventInitOK
)

// MatchVGPUSentinelLine extracts a complete guest-agent vGPU marker.
func MatchVGPUSentinelLine(line []byte) (marker string, event VGPUSentinelEvent) {
	if match := vgpuSentinelFailedPattern.FindSubmatch(line); match != nil {
		return string(match[1]), VGPUSentinelEventInitFailed
	}
	if match := vgpuSentinelOKPattern.FindSubmatch(line); match != nil {
		return string(match[1]), VGPUSentinelEventInitOK
	}
	return "", VGPUSentinelEventNone
}

type vgpuSentinelTarget struct {
	instanceID string
	vfAddress  string
	appLogPath string
	assignedAt string
}

type vgpuSentinelStore interface {
	listVGPUSentinelTargets(ctx context.Context) ([]vgpuSentinelTarget, error)
	getVGPUSentinelTarget(ctx context.Context, instanceID string) (vgpuSentinelTarget, bool, error)
}

var _ vgpuSentinelStore = (*manager)(nil)

type vgpuSentinelTail struct {
	vfAddress        string
	assignedAt       string
	offset           int64
	skippingLongLine bool
	pendingSuccess   bool
}

// VGPUSentinelController quarantines VFs reported as wedged by the guest agent.
type VGPUSentinelController struct {
	store             vgpuSentinelStore
	log               *slog.Logger
	interval          time.Duration
	reportFailure     func(devices.VFInitFailureReport) (devices.VFReportResult, error)
	reportSuccess     func(devices.VFInitSuccessReport) (devices.VFSuccessResult, error)
	guestGPUInitState func(ctx context.Context, instanceID string) (guest.GPUInitState, error)
	initFailures      metric.Int64Counter
	quarantines       metric.Int64Counter
	tails             map[string]*vgpuSentinelTail
	discoverFramework func() (devices.VGPUFramework, []devices.VirtualFunction, error)
	probeErrLoggedAt  time.Time
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
		interval:      vgpuSentinelScanInterval,
		reportFailure: devices.ReportVFInitFailure,
		reportSuccess: devices.ReportVFInitSuccess,
		guestGPUInitState: func(ctx context.Context, instanceID string) (guest.GPUInitState, error) {
			dialer, err := manager.GetVsockDialer(ctx, instanceID)
			if err != nil {
				return guest.GPUInitState_GPU_INIT_STATE_UNKNOWN, err
			}
			return guest.GetGPUInitStatus(ctx, dialer)
		},
		discoverFramework: devices.DiscoverVGPU,
		initFailures:      initFailures,
		quarantines:       quarantines,
		tails:             make(map[string]*vgpuSentinelTail),
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
			c.scanOnce(ctx)
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

func (c *VGPUSentinelController) scanOnce(ctx context.Context) {
	targets, err := c.store.listVGPUSentinelTargets(ctx)
	if err != nil {
		c.log.WarnContext(ctx, "vGPU sentinel scan failed to list instances", "error", err)
		return
	}
	alive := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		alive[target.instanceID] = struct{}{}
		c.scanTarget(ctx, target)
	}
	for id := range c.tails {
		if _, ok := alive[id]; !ok {
			delete(c.tails, id)
		}
	}
}

func (c *VGPUSentinelController) scanTarget(ctx context.Context, target vgpuSentinelTarget) {
	tail := c.tails[target.instanceID]
	if tail == nil || tail.vfAddress != target.vfAddress || tail.assignedAt != target.assignedAt {
		if tail != nil && tail.pendingSuccess {
			// Last chance to retry the previous assignment's clear before its
			// tail is replaced.
			c.retryPendingSuccess(ctx, target.instanceID, tail)
		}
		tail = &vgpuSentinelTail{vfAddress: target.vfAddress, assignedAt: target.assignedAt}
		c.tails[target.instanceID] = tail
	}
	if tail.pendingSuccess {
		c.retryPendingSuccess(ctx, target.instanceID, tail)
	}

	// Only the latest marker in a pass matters: a FAILED with a later OK is a
	// resolved pair (e.g. replayed after a restart) and must not be recorded.
	var lastLine string
	lastEvent := VGPUSentinelEventNone
	err := scanSentinelLog(target.appLogPath, tail, func(line string, event VGPUSentinelEvent) {
		lastLine, lastEvent = line, event
	})
	if err != nil {
		c.log.WarnContext(ctx, "vGPU sentinel scan failed to read app log",
			"instance_id", target.instanceID, "error", err)
	}
	switch lastEvent {
	case VGPUSentinelEventInitFailed:
		c.handleFailure(ctx, target, lastLine)
	case VGPUSentinelEventInitOK:
		c.processSuccess(ctx, target, tail)
	}
}

// corroborate accepts a marker only when the guest agent reports the matching
// init state over vsock. Markers share the serial console with workload
// output, so a bare log line does not establish that the guest agent wrote it.
func (c *VGPUSentinelController) corroborate(ctx context.Context, target vgpuSentinelTarget, want guest.GPUInitState) bool {
	ctx, cancel := context.WithTimeout(ctx, vgpuSentinelCorroborateTimeout)
	defer cancel()
	state, err := c.guestGPUInitState(ctx, target.instanceID)
	if err != nil {
		c.log.WarnContext(ctx, "vGPU sentinel cannot corroborate a marker with the guest agent; ignoring it",
			"vf", target.vfAddress, "instance_id", target.instanceID, "error", err)
		return false
	}
	if state != want {
		c.log.WarnContext(ctx, "vGPU sentinel ignoring a marker the guest agent does not corroborate",
			"vf", target.vfAddress, "instance_id", target.instanceID, "guest_state", state.String())
		return false
	}
	return true
}

// confirmAssignment rejects a marker only when the instance now holds a
// different VF assignment. Released assignments remain attributable to the
// assignment captured in the scan target.
func (c *VGPUSentinelController) confirmAssignment(ctx context.Context, target vgpuSentinelTarget) (bool, error) {
	current, ok, err := c.store.getVGPUSentinelTarget(ctx, target.instanceID)
	if err != nil {
		return false, err
	}
	return !ok || (current.vfAddress == target.vfAddress && current.assignedAt == target.assignedAt), nil
}

func (c *VGPUSentinelController) handleFailure(ctx context.Context, target vgpuSentinelTarget, line string) {
	unchanged, err := c.confirmAssignment(ctx, target)
	if err != nil {
		c.log.WarnContext(ctx, "vGPU sentinel could not confirm assignment before recording an init failure",
			"vf", target.vfAddress, "instance_id", target.instanceID, "error", err)
		return
	}
	if !unchanged {
		c.log.InfoContext(ctx, "vGPU sentinel skipping init failure: assignment changed during scan",
			"vf", target.vfAddress, "instance_id", target.instanceID)
		return
	}
	if !c.corroborate(ctx, target, guest.GPUInitState_GPU_INIT_STATE_FAILED) {
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
			"sentinel_line", line,
			"failures", result.Failures,
			"threshold", result.Threshold,
		)
		c.initFailures.Add(ctx, 1)
		c.quarantines.Add(ctx, 1)
	case devices.VFReportRecorded:
		c.log.WarnContext(ctx, "recorded vGPU VF init failure below quarantine threshold",
			"vf", target.vfAddress,
			"instance_id", target.instanceID,
			"sentinel_line", line,
			"failures", result.Failures,
			"threshold", result.Threshold,
		)
		c.initFailures.Add(ctx, 1)
	}
}

func (c *VGPUSentinelController) processSuccess(ctx context.Context, target vgpuSentinelTarget, tail *vgpuSentinelTail) {
	unchanged, err := c.confirmAssignment(ctx, target)
	if err != nil {
		c.log.WarnContext(ctx, "vGPU sentinel could not confirm assignment before clearing init failures",
			"vf", target.vfAddress, "instance_id", target.instanceID, "error", err)
		return
	}
	if !unchanged {
		return
	}
	if !c.corroborate(ctx, target, guest.GPUInitState_GPU_INIT_STATE_OK) {
		return
	}

	result, err := c.reportSuccess(devices.VFInitSuccessReport{
		VFAddress:  target.vfAddress,
		InstanceID: target.instanceID,
		AssignedAt: target.assignedAt,
	})
	if err != nil {
		// The offset has already advanced past the once-per-boot OK marker, so
		// remember the clear and retry it on later scans.
		tail.pendingSuccess = true
		c.log.WarnContext(ctx, "vGPU sentinel failed to clear recorded init failures; will retry",
			"vf", target.vfAddress, "instance_id", target.instanceID, "error", err)
		return
	}
	c.logSuccessResult(ctx, target.vfAddress, target.instanceID, result)
}

// retryPendingSuccess retries a corroborated clear whose persist failed. The
// store only clears exact assignment matches, so no reconfirmation is needed.
func (c *VGPUSentinelController) retryPendingSuccess(ctx context.Context, instanceID string, tail *vgpuSentinelTail) {
	result, err := c.reportSuccess(devices.VFInitSuccessReport{
		VFAddress:  tail.vfAddress,
		InstanceID: instanceID,
		AssignedAt: tail.assignedAt,
	})
	if err != nil {
		c.log.WarnContext(ctx, "vGPU sentinel failed to clear recorded init failures; will retry",
			"vf", tail.vfAddress, "instance_id", instanceID, "error", err)
		return
	}
	tail.pendingSuccess = false
	c.logSuccessResult(ctx, tail.vfAddress, instanceID, result)
}

func (c *VGPUSentinelController) logSuccessResult(ctx context.Context, vfAddress, instanceID string, result devices.VFSuccessResult) {
	if result.Rescinded {
		c.log.InfoContext(ctx, "rescinded vGPU VF quarantine after successful driver init from the triggering assignment",
			"vf", vfAddress, "instance_id", instanceID, "cleared", result.Cleared)
	} else if result.Cleared > 0 {
		c.log.InfoContext(ctx, "cleared recorded vGPU VF init failures after successful driver init",
			"vf", vfAddress, "instance_id", instanceID, "cleared", result.Cleared)
	}
}

func scanSentinelLog(path string, tail *vgpuSentinelTail, handle func(string, VGPUSentinelEvent)) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() < tail.offset {
		// Copy-truncate rotation moved unread bytes to the .1 backup; scan the
		// remainder there before restarting at the head of the active file.
		scanRotatedBackup(path+".1", tail, handle)
		tail.offset = 0
		tail.skippingLongLine = false
	}
	if _, err := f.Seek(tail.offset, io.SeekStart); err != nil {
		return err
	}

	consumed, skipping, err := readSentinelLines(f, tail.skippingLongLine, handle)
	tail.offset += consumed
	tail.skippingLongLine = skipping
	return err
}

func scanRotatedBackup(path string, tail *vgpuSentinelTail, handle func(string, VGPUSentinelEvent)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() < tail.offset {
		// The backup does not contain the unread tail (e.g. the log was
		// archived for a new boot rather than rotated).
		return
	}
	if _, err := f.Seek(tail.offset, io.SeekStart); err != nil {
		return
	}
	_, _, _ = readSentinelLines(f, tail.skippingLongLine, handle)
}

func readSentinelLines(r io.Reader, skipping bool, handle func(string, VGPUSentinelEvent)) (consumed int64, stillSkipping bool, err error) {
	reader := bufio.NewReaderSize(r, vgpuSentinelMaxLineBytes)
	for {
		line, err := reader.ReadSlice('\n')
		switch {
		case err == nil:
			consumed += int64(len(line))
			if skipping {
				skipping = false
				continue
			}
			marker, event := MatchVGPUSentinelLine(line)
			if event != VGPUSentinelEventNone {
				handle(marker, event)
			}
		case errors.Is(err, bufio.ErrBufferFull):
			consumed += int64(len(line))
			skipping = true
		case errors.Is(err, io.EOF):
			if skipping {
				consumed += int64(len(line))
			}
			return consumed, skipping, nil
		default:
			return consumed, skipping, err
		}
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
	if meta.GPUFramework != devices.VGPUFrameworkVendorVFIO || meta.GPUDevicePath == "" {
		return vgpuSentinelTarget{}, false, nil
	}
	assignedAt := ""
	if meta.GPUAssignedAt != nil {
		assignedAt = meta.GPUAssignedAt.UTC().Format(time.RFC3339Nano)
	}
	return vgpuSentinelTarget{
		instanceID: instanceID,
		vfAddress:  filepath.Base(meta.GPUDevicePath),
		appLogPath: m.paths.InstanceAppLog(instanceID),
		assignedAt: assignedAt,
	}, true, nil
}
