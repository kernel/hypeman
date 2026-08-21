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
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/logger"
	"go.opentelemetry.io/otel/metric"
)

const (
	vgpuSentinelScanInterval = 5 * time.Second

	// vgpuSentinelMaxLineBytes bounds how much of any single log line a scan
	// holds in memory. The log is guest-controlled console output; marker
	// lines are a few hundred bytes, so anything longer is console spam and
	// is discarded — including its later-arriving tail — without ever being
	// buffered whole.
	vgpuSentinelMaxLineBytes = 64 * 1024
)

// vgpuSentinelPattern matches the guest agent's HYPEMAN-GPU-INIT-FAILED
// report as it arrives on the serial console (the agent observes the NVIDIA
// driver's RmInitAdapter failure in the guest kernel log and emits this
// marker — the same guest-to-host channel as the other HYPEMAN-* markers).
// The full marker shape through the quoted NVRM payload is required: a bare
// token or a truncated marker could appear in echoed exec command lines.
var vgpuSentinelPattern = regexp.MustCompile(`HYPEMAN-GPU-INIT-FAILED ts=\S+ nvrm="NVRM: [^"]*RmInitAdapter failed![^"]*"`)

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
	// skippingLongLine marks that offset sits inside a line that exceeded
	// vgpuSentinelMaxLineBytes; content is discarded until its newline so an
	// oversized line's tail is never parsed as a fresh line.
	skippingLongLine bool
	done             bool
}

// VGPUSentinelController scans the serial console log of vendor VFIO vGPU
// instances for the guest driver's RmInitAdapter failure line. That line is a
// positive fingerprint of a wedged VF — the driver is present, trying, and
// failing — so a match quarantines the VF, removing it from placement until
// an operator cycles the parent GPU.
type VGPUSentinelController struct {
	store       vgpuSentinelStore
	log         *slog.Logger
	interval    time.Duration
	quarantine  func(devices.VFQuarantine) (bool, error)
	convictions metric.Int64Counter
	tails       map[string]*vgpuSentinelTail
	// discoverFramework is overridden in tests; nil means devices.DiscoverVGPU.
	discoverFramework func() (devices.VGPUFramework, []devices.VirtualFunction, error)
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
		metric.WithDescription("Total wedged-VF sentinel convictions"),
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
	// The quarantined-VFs gauge reads zero when the state file is unreadable
	// — exactly the state where quarantines exist but are unloadable and vGPU
	// placement is failing closed — so that condition gets its own signal.
	_, err = meter.Int64ObservableGauge(
		"hypeman_instances_vgpu_vf_health_store_unavailable",
		metric.WithDescription("1 when the persisted VF health state failed to load; quarantine mutations are refused and vGPU placement is disabled until it is repaired"),
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
		store:       store,
		log:         log.With("controller", "vgpu_sentinel"),
		interval:    vgpuSentinelScanInterval,
		quarantine:  devices.QuarantineVF,
		convictions: convictions,
		tails:       make(map[string]*vgpuSentinelTail),
	}, nil
}

func (c *VGPUSentinelController) Run(ctx context.Context) error {
	// Wedges only exist on vendor VFIO hosts; scanning instance metadata
	// every interval on mdev and CPU-only hosts would be pure overhead. The
	// framework is fixed for the lifetime of the process (SR-IOV provisioning
	// precedes hypeman startup), so one successful probe settles the gate. A
	// failed probe fails open — a transient discovery error must not disable
	// detection on a vendor VFIO host — and is retried each tick so one bad
	// probe cannot leave a non-GPU host scanning for the process lifetime.
	vendor, known := c.probeVendorVFIO()
	if known && !vendor {
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
			if !known {
				if vendor, known = c.probeVendorVFIO(); known && !vendor {
					return nil
				}
			}
			c.scanOnce(ctx)
		}
	}
}

// probeVendorVFIO reports whether the host's vGPU framework could be
// determined and, when known, whether it is vendor VFIO. It logs the idle
// verdict so every exit path announces why the controller stopped.
func (c *VGPUSentinelController) probeVendorVFIO() (vendor, known bool) {
	discover := c.discoverFramework
	if discover == nil {
		discover = devices.DiscoverVGPU
	}
	framework, _, err := discover()
	if err != nil {
		return false, false
	}
	if framework != devices.VGPUFrameworkVendorVFIO {
		c.log.Info("vGPU sentinel controller idle: host has no vendor VFIO framework", "framework", string(framework))
		return false, true
	}
	return true, true
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
	line, found, err := scanForSentinel(target.appLogPath, tail)
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

// convict quarantines the target's VF. It reports whether scanning for this
// instance is finished; a failed quarantine leaves the tail open so the
// recurring report retries it.
func (c *VGPUSentinelController) convict(ctx context.Context, target vgpuSentinelTarget, line string) bool {
	existed, err := c.quarantine(devices.VFQuarantine{
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
		// Already out of placement — typically a rescan of a standing
		// victim's log after a controller restart. Not a new wedge: no
		// metric.
		c.log.InfoContext(ctx, "vGPU sentinel matched an already-quarantined VF",
			"vf", target.vfAddress, "instance_id", target.instanceID)
		return true
	}
	c.log.ErrorContext(ctx, "quarantined wedged vGPU VF",
		"vf", target.vfAddress,
		"instance_id", target.instanceID,
		"sentinel_line", line,
	)
	c.convictions.Add(ctx, 1)
	return true
}

// scanForSentinel reads complete lines from the tail's offset onward,
// advancing the offset and returning the first sentinel match — only the
// matched marker, not the whole line: the surrounding bytes are
// guest-controlled console output up to the line cap and do not belong in
// error logs or the persisted health state. A partial
// trailing line is left unconsumed for the next scan. An offset past the
// file size means the log was archived for a new boot, so the scan restarts
// from the top. Memory is bounded by the line cap: a line that overflows the
// read buffer cannot be a marker, so it is consumed — across scans if its
// newline has not arrived yet — without ever being held whole.
func scanForSentinel(path string, tail *vgpuSentinelTail) (string, bool, error) {
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
	if info.Size() < tail.offset {
		tail.offset = 0
		tail.skippingLongLine = false
	}
	if _, err := f.Seek(tail.offset, io.SeekStart); err != nil {
		return "", false, err
	}

	reader := bufio.NewReaderSize(f, vgpuSentinelMaxLineBytes)
	for {
		line, err := reader.ReadSlice('\n')
		switch {
		case err == nil:
			tail.offset += int64(len(line))
			if tail.skippingLongLine {
				tail.skippingLongLine = false
				continue
			}
			if match := vgpuSentinelPattern.Find(line); match != nil {
				return string(match), true, nil
			}
		case errors.Is(err, bufio.ErrBufferFull):
			tail.offset += int64(len(line))
			tail.skippingLongLine = true
		case errors.Is(err, io.EOF):
			// A partial trailing line stays unconsumed for the next scan,
			// unless it is the tail of an oversized line being discarded.
			if tail.skippingLongLine {
				tail.offset += int64(len(line))
			}
			return "", false, nil
		default:
			return "", false, err
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
			if !errors.Is(err, ErrNotFound) {
				// An unreadable record removes its VF from detection; say so
				// rather than silently shrinking coverage.
				logger.FromContext(ctx).WarnContext(ctx, "vGPU sentinel skipping unreadable instance metadata", "instance_id", id, "error", err)
			}
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
