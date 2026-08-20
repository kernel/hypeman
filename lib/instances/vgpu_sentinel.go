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
)

// vgpuSentinelPattern matches the guest NVIDIA driver's init-failure line as
// it arrives on the serial console. The full "NVRM: ... RmInitAdapter
// failed!" shape is required: the bare token also appears in echoed exec
// command lines, and the trailing (stage:status:line) tuple is
// driver-build-specific, so neither is safe to match.
var vgpuSentinelPattern = regexp.MustCompile(`NVRM: .*RmInitAdapter failed!`)

type vgpuSentinelTarget struct {
	instanceID string
	vfAddress  string
	appLogPath string
}

type vgpuSentinelStore interface {
	listVGPUSentinelTargets(ctx context.Context) ([]vgpuSentinelTarget, error)
}

type vgpuSentinelTail struct {
	offset int64
	done   bool
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
	now         func() time.Time
	quarantine  func(devices.VFQuarantine) (devices.VFHealthRecord, error)
	convictions metric.Int64Counter
	tails       map[string]*vgpuSentinelTail
	recent      []time.Time
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
		store:       store,
		log:         log.With("controller", "vgpu_sentinel"),
		interval:    vgpuSentinelScanInterval,
		now:         time.Now,
		quarantine:  devices.QuarantineVF,
		convictions: convictions,
		tails:       make(map[string]*vgpuSentinelTail),
	}, nil
}

func (c *VGPUSentinelController) Run(ctx context.Context) error {
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
	if tail == nil {
		tail = &vgpuSentinelTail{}
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
// It reports whether scanning for this instance is finished; a failed
// quarantine leaves the tail open so the recurring sentinel retries it.
func (c *VGPUSentinelController) convict(ctx context.Context, target vgpuSentinelTarget, line string) bool {
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
		return true
	}

	record, err := c.quarantine(devices.VFQuarantine{
		VFAddress:    target.vfAddress,
		InstanceID:   target.instanceID,
		SentinelLine: line,
	})
	if err != nil {
		c.log.ErrorContext(ctx, "failed to quarantine wedged vGPU VF",
			"vf", target.vfAddress, "instance_id", target.instanceID, "error", err)
		return false
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
		targets = append(targets, vgpuSentinelTarget{
			instanceID: id,
			vfAddress:  filepath.Base(meta.GPUDevicePath),
			appLogPath: m.paths.InstanceAppLog(id),
		})
	}
	return targets, nil
}
