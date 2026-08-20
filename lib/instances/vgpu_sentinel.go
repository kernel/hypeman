package instances

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	vgpuSentinelScanInterval = 5 * time.Second

	// A burst of convictions is more likely a systemic, non-wedge init
	// failure (e.g. a guest/host driver mismatch rolling out fleet-wide)
	// than several independent wedges; quarantining every VF would degrade
	// the host harder than the failure itself, so auto-conviction pauses.
	vgpuSentinelBrakeWindow = 15 * time.Minute
	vgpuSentinelBrakeLimit  = 3

	// vgpuSentinelMaxLineBytes bounds how much of an unterminated trailing
	// line a scan will buffer and re-read. Marker lines are short; anything
	// this long is console spam and is skipped once complete reading it
	// becomes unreasonable.
	vgpuSentinelMaxLineBytes = 64 * 1024
)

// vgpuSentinelPattern matches the guest agent's HYPEMAN-GPU-INIT-FAILED
// report as it arrives on the serial console (the agent observes the NVIDIA
// driver's RmInitAdapter failure in the guest kernel log and emits this
// marker — the same guest-to-host channel as the other HYPEMAN-* markers).
// The full marker shape including the nvrm field is required: a bare token
// could appear in echoed exec command lines.
var vgpuSentinelPattern = regexp.MustCompile(`HYPEMAN-GPU-INIT-FAILED ts=\S+ nvrm="`)

type vgpuSentinelTarget struct {
	instanceID string
	vfAddress  string
	appLogPath string
	// assignedAt identifies one assignment epoch: it changes whenever the
	// instance acquires a vGPU, so a tail from a previous boot or a previous
	// VF is never carried into the next one.
	assignedAt string
}

type vgpuSentinelStore interface {
	listVGPUSentinelTargets(ctx context.Context) ([]vgpuSentinelTarget, error)
}

type vgpuSentinelTail struct {
	vfAddress  string
	assignedAt string
	offset     int64
	done       bool
}

// VGPUSentinelController scans the serial console log of vendor VFIO vGPU
// instances for the guest driver's RmInitAdapter failure line. That line is a
// positive fingerprint of a wedged VF — the driver is present, trying, and
// failing — so a match quarantines the VF, removing it from placement until
// an operator cycles the parent GPU.
type VGPUSentinelController struct {
	store         vgpuSentinelStore
	log           *slog.Logger
	interval      time.Duration
	now           func() time.Time
	quarantine    func(devices.VFQuarantine) (devices.VFHealthRecord, bool, error)
	isQuarantined func(string) bool
	hostFramework func() devices.VGPUFramework
	convictions   metric.Int64Counter
	tails         map[string]*vgpuSentinelTail
	recent        []time.Time
}

func NewVGPUSentinelController(manager Manager, meter metric.Meter, log *slog.Logger) (*VGPUSentinelController, error) {
	if manager == nil || log == nil {
		return nil, nil
	}
	store, ok := manager.(vgpuSentinelStore)
	if !ok {
		return nil, nil
	}

	convictions, err := meter.Int64Counter(
		"hypeman_instances_vgpu_sentinel_convictions_total",
		metric.WithDescription("Total wedged-VF sentinel matches by result"),
	)
	if err != nil {
		return nil, err
	}
	_, err = meter.Int64ObservableGauge(
		"hypeman_instances_vgpu_quarantined_vfs",
		metric.WithDescription("Number of vGPU virtual functions currently quarantined"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(len(devices.QuarantinedVFs())))
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
		now:           time.Now,
		quarantine:    devices.QuarantineVF,
		isQuarantined: devices.IsVFQuarantined,
		hostFramework: func() devices.VGPUFramework {
			framework, _, err := devices.DiscoverVGPU()
			if err != nil {
				// Fail open: a transient discovery error must not disable
				// detection on a vendor VFIO host.
				return devices.VGPUFrameworkVendorVFIO
			}
			return framework
		},
		convictions: convictions,
		tails:       make(map[string]*vgpuSentinelTail),
	}, nil
}

func (c *VGPUSentinelController) Run(ctx context.Context) error {
	// Wedges only exist on vendor VFIO hosts; scanning instance metadata
	// every interval on mdev and CPU-only hosts would be pure overhead. The
	// framework is fixed for the lifetime of the process (SR-IOV provisioning
	// precedes hypeman startup), so this is a one-time gate.
	if framework := c.hostFramework(); framework != devices.VGPUFrameworkVendorVFIO {
		c.log.Info("vGPU sentinel controller idle: host has no vendor VFIO framework", "framework", string(framework))
		return nil
	}
	c.log.Info("vGPU sentinel controller started")
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.scanOnce(ctx)
		}
	}
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
		// A new assignment (stop/start, possibly on a different VF) writes a
		// fresh log; a tail finished for the previous assignment must not
		// suppress scanning the new one.
		tail = &vgpuSentinelTail{vfAddress: target.vfAddress, assignedAt: target.assignedAt}
		c.tails[target.instanceID] = tail
	}
	if tail.done {
		return
	}
	line, found, err := scanForSentinel(target.appLogPath, &tail.offset)
	if err != nil {
		c.log.WarnContext(ctx, "vGPU sentinel scan failed to read app log",
			"instance_id", target.instanceID, "error", err)
		return
	}
	if !found {
		return
	}
	tail.done = c.convict(ctx, target, line)
}

// convict quarantines the target's VF, subject to the conviction brake.
// It reports whether scanning for this instance is finished; a failed or
// brake-suppressed quarantine leaves the tail open so the recurring report
// retries it — the brake pauses auto-conviction, it must not permanently
// drop a conviction.
func (c *VGPUSentinelController) convict(ctx context.Context, target vgpuSentinelTarget, line string) bool {
	if c.isQuarantined(target.vfAddress) {
		// Already out of placement — typically a rescan of a standing
		// victim's log after a controller restart. Not a new wedge: no
		// metric, no brake accounting.
		c.log.InfoContext(ctx, "vGPU sentinel matched an already-quarantined VF",
			"vf", target.vfAddress, "instance_id", target.instanceID)
		return true
	}

	now := c.now()
	recent := c.recent[:0]
	for _, t := range c.recent {
		if now.Sub(t) < vgpuSentinelBrakeWindow {
			recent = append(recent, t)
		}
	}
	c.recent = recent

	if len(c.recent) >= vgpuSentinelBrakeLimit {
		c.log.ErrorContext(ctx, "vGPU sentinel conviction brake engaged; VF not quarantined",
			"vf", target.vfAddress,
			"instance_id", target.instanceID,
			"sentinel_line", line,
			"convictions_in_window", len(c.recent),
			"window", vgpuSentinelBrakeWindow.String(),
		)
		c.convictions.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "suppressed")))
		return false
	}

	record, existed, err := c.quarantine(devices.VFQuarantine{
		VFAddress:    target.vfAddress,
		InstanceID:   target.instanceID,
		SentinelLine: line,
	})
	if err != nil {
		c.log.ErrorContext(ctx, "failed to quarantine wedged vGPU VF",
			"vf", target.vfAddress, "instance_id", target.instanceID, "error", err)
		return false
	}
	if existed {
		// Lost a conviction race; the VF is already quarantined.
		return true
	}
	c.recent = append(c.recent, now)
	c.log.ErrorContext(ctx, "quarantined wedged vGPU VF",
		"vf", target.vfAddress,
		"instance_id", target.instanceID,
		"sentinel_line", line,
		"wedge_count", record.WedgeCount,
	)
	c.convictions.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "convicted")))
	return true
}

// scanForSentinel reads complete lines from offset onward, advancing offset
// and returning the first sentinel match. A partial trailing line is left
// unconsumed for the next scan. An offset past the file size means the log
// was archived for a new boot, so the scan restarts from the top.
func scanForSentinel(path string, offset *int64) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	if info.Size() < *offset {
		*offset = 0
	}
	if _, err := f.Seek(*offset, io.SeekStart); err != nil {
		return "", false, err
	}

	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				// An oversized unterminated line would otherwise be buffered
				// again on every scan; skip it once it exceeds the bound.
				if len(line) > vgpuSentinelMaxLineBytes {
					*offset += int64(len(line))
				}
				return "", false, nil
			}
			return "", false, err
		}
		*offset += int64(len(line))
		if vgpuSentinelPattern.MatchString(line) {
			return strings.TrimSpace(line), true, nil
		}
	}
}

// listVGPUSentinelTargets returns instances holding a vendor VFIO vGPU
// assignment. It reads raw metadata rather than hydrating full instances:
// the scan runs continuously and deriving state would query every
// hypervisor on the host.
func (m *manager) listVGPUSentinelTargets(ctx context.Context) ([]vgpuSentinelTarget, error) {
	files, err := m.listMetadataFiles()
	if err != nil {
		return nil, err
	}
	targets := make([]vgpuSentinelTarget, 0, len(files))
	for _, file := range files {
		id := filepath.Base(filepath.Dir(file))
		meta, err := m.loadMetadata(id)
		if err != nil {
			continue
		}
		if meta.GPUFramework != devices.VGPUFrameworkVendorVFIO || meta.GPUDevicePath == "" {
			continue
		}
		assignedAt := ""
		if meta.GPUAssignedAt != nil {
			assignedAt = meta.GPUAssignedAt.UTC().Format(time.RFC3339Nano)
		}
		targets = append(targets, vgpuSentinelTarget{
			instanceID: id,
			vfAddress:  filepath.Base(meta.GPUDevicePath),
			appLogPath: m.paths.InstanceAppLog(id),
			assignedAt: assignedAt,
		})
	}
	return targets, nil
}
