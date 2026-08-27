package instances

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"
	otelmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const testNVRMMessage = "NVRM: GPU 0000:e3:00.4: RmInitAdapter failed! (0x22:0x65:884)"

type fakeSentinelStore struct {
	targets   []vgpuSentinelTarget
	listCalls int
}

func (s *fakeSentinelStore) listVGPUSentinelTargets(context.Context) ([]vgpuSentinelTarget, error) {
	s.listCalls++
	return s.targets, nil
}

func (s *fakeSentinelStore) getVGPUSentinelTarget(_ context.Context, instanceID string) (vgpuSentinelTarget, bool, error) {
	for _, target := range s.targets {
		if target.instanceID == instanceID {
			return target, true, nil
		}
	}
	return vgpuSentinelTarget{}, false, nil
}

func TestNewVGPUSentinelControllerRejectsUnsupportedManager(t *testing.T) {
	_, err := NewVGPUSentinelController(&stubManager{}, noop.NewMeterProvider().Meter("test"), slog.New(slog.DiscardHandler))
	require.ErrorContains(t, err, "does not implement vgpuSentinelStore")
}

func guestReportsFailed(context.Context, string) (guest.GPUInitState, string, error) {
	return guest.GPUInitState_GPU_INIT_STATE_FAILED, testNVRMMessage, nil
}

func guestReportsOK(context.Context, string) (guest.GPUInitState, string, error) {
	return guest.GPUInitState_GPU_INIT_STATE_OK, "", nil
}

func newTestSentinelController(t *testing.T, store *fakeSentinelStore) (*VGPUSentinelController, *[]devices.VFInitFailureReport) {
	t.Helper()
	counter, err := noop.NewMeterProvider().Meter("test").Int64Counter("test")
	require.NoError(t, err)
	var reported []devices.VFInitFailureReport
	c := &VGPUSentinelController{
		store:    store,
		log:      slog.New(slog.DiscardHandler),
		interval: time.Hour,
		// Mirrors the real store: repeated reports for the same assignment are
		// deduplicated.
		reportFailure: func(report devices.VFInitFailureReport) (devices.VFReportResult, error) {
			for _, previous := range reported {
				if previous == report {
					return devices.VFReportResult{Outcome: devices.VFReportUnchanged, Failures: 1, Threshold: 1}, nil
				}
			}
			reported = append(reported, report)
			return devices.VFReportResult{Outcome: devices.VFReportQuarantined, Failures: 1, Threshold: 1}, nil
		},
		reportSuccess: func(devices.VFInitSuccessReport) (devices.VFSuccessResult, error) {
			return devices.VFSuccessResult{}, nil
		},
		guestGPUInitStatus: guestReportsFailed,
		initFailures:       counter,
		quarantines:        counter,
		checks:             counter,
	}
	return c, &reported
}

func TestVGPUSentinelControllerReportsFailureOnce(t *testing.T) {
	t.Parallel()

	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		assignedAt: "2026-08-20T15:00:00Z",
	}}}
	c, reported := newTestSentinelController(t, store)
	ctx := context.Background()

	c.guestGPUInitStatus = func(context.Context, string) (guest.GPUInitState, string, error) {
		return guest.GPUInitState_GPU_INIT_STATE_UNKNOWN, "", nil
	}
	c.pollOnce(ctx)
	assert.Empty(t, *reported, "an undecided init must not be reported")

	c.guestGPUInitStatus = guestReportsFailed
	c.pollOnce(ctx)
	require.Len(t, *reported, 1)
	assert.Equal(t, "0000:e3:00.4", (*reported)[0].VFAddress)
	assert.Equal(t, "instance-1", (*reported)[0].InstanceID)

	c.pollOnce(ctx)
	assert.Len(t, *reported, 1, "repeated polls of the same failed assignment must deduplicate")
}

func TestVGPUSentinelControllerSkipsUnreachableGuest(t *testing.T) {
	t.Parallel()

	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		assignedAt: "2026-08-20T15:00:00Z",
	}}}
	c, reported := newTestSentinelController(t, store)
	c.guestGPUInitStatus = func(context.Context, string) (guest.GPUInitState, string, error) {
		return guest.GPUInitState_GPU_INIT_STATE_UNKNOWN, "", errors.New("vsock dial failed")
	}
	var successes []devices.VFInitSuccessReport
	c.reportSuccess = func(report devices.VFInitSuccessReport) (devices.VFSuccessResult, error) {
		successes = append(successes, report)
		return devices.VFSuccessResult{}, nil
	}

	c.pollOnce(context.Background())
	assert.Empty(t, *reported)
	assert.Empty(t, successes)
}

func TestVGPUSentinelControllerPollsTargetsConcurrently(t *testing.T) {
	t.Parallel()

	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{
		{instanceID: "instance-1"},
		{instanceID: "instance-2"},
	}}
	c, _ := newTestSentinelController(t, store)
	started := make(chan struct{}, len(store.targets))
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	c.guestGPUInitStatus = func(context.Context, string) (guest.GPUInitState, string, error) {
		started <- struct{}{}
		<-release
		return guest.GPUInitState_GPU_INIT_STATE_UNKNOWN, "", nil
	}

	done := make(chan struct{})
	go func() {
		c.pollOnce(context.Background())
		close(done)
	}()
	for range store.targets {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("targets were not polled concurrently")
		}
	}
	close(release)
	released = true
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poll did not finish")
	}
}

func TestVGPUSentinelControllerRecordsCheckResults(t *testing.T) {
	t.Parallel()

	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{
		{instanceID: "ok"},
		{instanceID: "unknown"},
		{instanceID: "unreachable"},
	}}
	c, _ := newTestSentinelController(t, store)
	reader := otelmetric.NewManualReader()
	provider := otelmetric.NewMeterProvider(otelmetric.WithReader(reader))
	checks, err := provider.Meter("test").Int64Counter("hypeman_instances_vgpu_sentinel_checks_total")
	require.NoError(t, err)
	c.checks = checks
	c.guestGPUInitStatus = func(_ context.Context, instanceID string) (guest.GPUInitState, string, error) {
		switch instanceID {
		case "ok":
			return guest.GPUInitState_GPU_INIT_STATE_OK, "", nil
		case "unknown":
			return guest.GPUInitState_GPU_INIT_STATE_UNKNOWN, "", nil
		default:
			return guest.GPUInitState_GPU_INIT_STATE_UNKNOWN, "", errors.New("vsock dial failed")
		}
	}

	c.pollOnce(context.Background())
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	metric := findMetric(t, rm, "hypeman_instances_vgpu_sentinel_checks_total")
	checksTotal, ok := metric.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	got := make(map[string]int64)
	for _, point := range checksTotal.DataPoints {
		got[metricLabel(t, point.Attributes, "result")] = point.Value
	}
	assert.Equal(t, map[string]int64{"ok": 1, "unknown": 1, "rpc_error": 1}, got)
}

func TestVGPUSentinelControllerProcessesSuccessAfterFailure(t *testing.T) {
	tests := []struct {
		name    string
		failure devices.VFReportResult
		success devices.VFSuccessResult
	}{
		{
			name:    "recorded",
			failure: devices.VFReportResult{Outcome: devices.VFReportRecorded, Failures: 1, Threshold: 2},
			success: devices.VFSuccessResult{Cleared: 1},
		},
		{
			name:    "quarantined",
			failure: devices.VFReportResult{Outcome: devices.VFReportQuarantined, Failures: 2, Threshold: 2},
			success: devices.VFSuccessResult{Cleared: 2, Rescinded: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
				instanceID: "instance-1",
				vfAddress:  "0000:e3:00.4",
				assignedAt: "2026-08-20T15:00:00Z",
			}}}
			c, reported := newTestSentinelController(t, store)
			c.reportFailure = func(report devices.VFInitFailureReport) (devices.VFReportResult, error) {
				*reported = append(*reported, report)
				return tt.failure, nil
			}
			var successes []devices.VFInitSuccessReport
			c.reportSuccess = func(report devices.VFInitSuccessReport) (devices.VFSuccessResult, error) {
				successes = append(successes, report)
				return tt.success, nil
			}

			c.pollOnce(context.Background())
			require.Len(t, *reported, 1)
			require.Empty(t, successes)

			// A wedge that recovers (e.g. driver reload) flips the guest state.
			c.guestGPUInitStatus = guestReportsOK
			c.pollOnce(context.Background())
			require.Len(t, *reported, 1)
			require.Len(t, successes, 1)
			assert.Equal(t, "instance-1", successes[0].InstanceID)
		})
	}
}

func TestVGPUSentinelControllerRetriesFailedTallyClear(t *testing.T) {
	t.Parallel()

	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		assignedAt: "2026-08-20T15:00:00Z",
	}}}
	c, _ := newTestSentinelController(t, store)
	c.guestGPUInitStatus = guestReportsOK
	var logs bytes.Buffer
	c.log = slog.New(slog.NewTextHandler(&logs, nil))
	var reports []devices.VFInitSuccessReport
	c.reportSuccess = func(report devices.VFInitSuccessReport) (devices.VFSuccessResult, error) {
		reports = append(reports, report)
		if len(reports) == 1 {
			return devices.VFSuccessResult{}, errors.New("persist failed")
		}
		return devices.VFSuccessResult{Cleared: 1}, nil
	}

	c.pollOnce(context.Background())
	require.Len(t, reports, 1)
	assert.Contains(t, logs.String(), "persist failed")

	// The guest keeps reporting OK, so the next poll retries the clear.
	c.pollOnce(context.Background())
	require.Len(t, reports, 2)
	assert.Equal(t, "0000:e3:00.4", reports[1].VFAddress)
}

func TestVGPUSentinelControllerRetriesFailedQuarantine(t *testing.T) {
	t.Parallel()

	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		assignedAt: "2026-08-20T15:00:00Z",
	}}}
	c, reported := newTestSentinelController(t, store)
	realReport := c.reportFailure
	c.reportFailure = func(devices.VFInitFailureReport) (devices.VFReportResult, error) {
		return devices.VFReportResult{}, errors.New("persist failed")
	}

	c.pollOnce(context.Background())
	assert.Empty(t, *reported)

	c.reportFailure = realReport
	c.pollOnce(context.Background())
	assert.Len(t, *reported, 1)
}

func TestVGPUSentinelControllerLogsNVRMMessageOnQuarantine(t *testing.T) {
	t.Parallel()

	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		assignedAt: "2026-08-20T15:00:00Z",
	}}}
	c, _ := newTestSentinelController(t, store)
	var logs bytes.Buffer
	c.log = slog.New(slog.NewTextHandler(&logs, nil))

	c.pollOnce(context.Background())
	assert.Contains(t, logs.String(), "quarantined wedged vGPU VF")
	assert.Contains(t, logs.String(), "RmInitAdapter failed!")
}

func TestVGPUSentinelControllerSkipsFailureOnChangedAssignment(t *testing.T) {
	t.Parallel()

	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.5",
		assignedAt: "2026-08-21T00:00:10Z",
	}}}
	c, reported := newTestSentinelController(t, store)

	stale := vgpuSentinelTarget{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		assignedAt: "2026-08-21T00:00:00Z",
	}
	c.pollTarget(context.Background(), stale)
	assert.Empty(t, *reported)

	// A released assignment (instance gone) remains attributable.
	store.targets = nil
	c.pollTarget(context.Background(), stale)
	require.Len(t, *reported, 1)
	assert.Equal(t, "0000:e3:00.4", (*reported)[0].VFAddress)
}

func TestVGPUSentinelControllerSkipsInitOKOnChangedAssignment(t *testing.T) {
	t.Parallel()

	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.5",
		assignedAt: "2026-08-21T00:00:10Z",
	}}}
	c, _ := newTestSentinelController(t, store)
	c.guestGPUInitStatus = guestReportsOK
	var cleared []devices.VFInitSuccessReport
	c.reportSuccess = func(report devices.VFInitSuccessReport) (devices.VFSuccessResult, error) {
		cleared = append(cleared, report)
		return devices.VFSuccessResult{Cleared: 1}, nil
	}

	c.pollTarget(context.Background(), vgpuSentinelTarget{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		assignedAt: "2026-08-21T00:00:00Z",
	})
	assert.Empty(t, cleared)
}

func TestVGPUSentinelControllerAppliesInitOKFromReleasedAssignment(t *testing.T) {
	t.Parallel()

	c, _ := newTestSentinelController(t, &fakeSentinelStore{})
	c.guestGPUInitStatus = guestReportsOK
	var cleared []devices.VFInitSuccessReport
	c.reportSuccess = func(report devices.VFInitSuccessReport) (devices.VFSuccessResult, error) {
		cleared = append(cleared, report)
		return devices.VFSuccessResult{Cleared: 1}, nil
	}

	c.pollTarget(context.Background(), vgpuSentinelTarget{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		assignedAt: "2026-08-21T00:00:00Z",
	})
	require.Len(t, cleared, 1)
	assert.Equal(t, "2026-08-21T00:00:00Z", cleared[0].AssignedAt)
}

func TestListVGPUSentinelTargetsSkipsUnstattableMetadata(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	m := &manager{paths: paths.New(t.TempDir())}
	assigned := time.Now().UTC()
	for _, id := range []string{"readable", "unreadable-a", "unreadable-b"} {
		require.NoError(t, m.ensureDirectories(id))
		require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
			Id:            id,
			GPUFramework:  devices.VGPUFrameworkVendorVFIO,
			GPUDevicePath: "/sys/bus/pci/devices/0000:e3:00.4",
			GPUAssignedAt: &assigned,
		}}))
	}
	for _, id := range []string{"unreadable-a", "unreadable-b"} {
		instanceDir := filepath.Dir(m.paths.InstanceMetadata(id))
		require.NoError(t, os.Chmod(instanceDir, 0o000))
		t.Cleanup(func() { _ = os.Chmod(instanceDir, 0o755) })
	}

	files, err := m.listMetadataFilesStrict()
	require.Error(t, err)
	assert.ErrorContains(t, err, "unreadable-a")
	assert.ErrorContains(t, err, "unreadable-b")
	require.Len(t, files, 1)

	var logs bytes.Buffer
	ctx := logger.AddToContext(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)))
	targets, err := m.listVGPUSentinelTargets(ctx)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	assert.Equal(t, "readable", targets[0].instanceID)
	assert.Contains(t, logs.String(), "vGPU sentinel cannot stat some instance metadata; their VFs are not scanned")
	assert.Contains(t, logs.String(), "unreadable-a")
	assert.Contains(t, logs.String(), "unreadable-b")
}

func TestVGPUSentinelControllerRunExitsWhenHostIsNotVendorVFIO(t *testing.T) {
	tests := []struct {
		name        string
		framework   devices.VGPUFramework
		firstError  bool
		wantProbes  int
		wantScans   int
		wantLogText string
	}{
		{
			name:        "missing framework",
			wantProbes:  1,
			wantLogText: "host has no vGPU framework",
		},
		{
			name:        "discovery retry resolves to mdev",
			framework:   devices.VGPUFrameworkMdev,
			firstError:  true,
			wantProbes:  2,
			wantScans:   1,
			wantLogText: "host vGPU framework is not vendor VFIO",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeSentinelStore{}
			c, _ := newTestSentinelController(t, store)
			c.interval = time.Millisecond
			var logs bytes.Buffer
			c.log = slog.New(slog.NewTextHandler(&logs, nil))
			var probes int
			c.discoverFramework = func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
				probes++
				if tt.firstError && probes == 1 {
					return devices.VGPUFrameworkNone, nil, errors.New("transient sysfs error")
				}
				return tt.framework, nil, nil
			}

			done := make(chan error, 1)
			go func() { done <- c.Run(context.Background()) }()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not exit")
			}
			assert.Equal(t, tt.wantProbes, probes)
			assert.Equal(t, tt.wantScans, store.listCalls)
			assert.NotContains(t, logs.String(), "vGPU sentinel controller started")
			assert.Contains(t, logs.String(), tt.wantLogText)
		})
	}
}
