package instances

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// sentinelReports records what the controller handed to the health store.
// Targets are polled concurrently, so appends are serialized.
type sentinelReports struct {
	mu        sync.Mutex
	failures  []devices.VFInitFailureReport
	successes []devices.VFInitSuccessReport
}

func (r *sentinelReports) recordFailure(report devices.VFInitFailureReport) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, report)
	return len(r.failures)
}

func (r *sentinelReports) recordSuccess(report devices.VFInitSuccessReport) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.successes = append(r.successes, report)
	return len(r.successes)
}

func TestNewVGPUSentinelControllerRejectsUnsupportedManager(t *testing.T) {
	_, err := NewVGPUSentinelController(&stubManager{}, noop.NewMeterProvider().Meter("test"), slog.New(slog.DiscardHandler))
	require.ErrorContains(t, err, "does not implement vgpuSentinelStore")
}

func guestReportsFailed(context.Context, vgpuSentinelTarget) (guest.GPUInitState, string, error) {
	return guest.GPUInitState_GPU_INIT_STATE_FAILED, testNVRMMessage, nil
}

func guestReportsOK(context.Context, vgpuSentinelTarget) (guest.GPUInitState, string, error) {
	return guest.GPUInitState_GPU_INIT_STATE_OK, "", nil
}

func newTestSentinelController(t *testing.T, store *fakeSentinelStore) (*VGPUSentinelController, *sentinelReports) {
	t.Helper()
	counter, err := noop.NewMeterProvider().Meter("test").Int64Counter("test")
	require.NoError(t, err)
	reports := &sentinelReports{}
	c := &VGPUSentinelController{
		store:             store,
		log:               slog.New(slog.DiscardHandler),
		interval:          time.Hour,
		repairHealthStore: func() error { return nil },
		reportFailure: func(report devices.VFInitFailureReport) (devices.VFReportResult, error) {
			reports.recordFailure(report)
			return devices.VFReportResult{Outcome: devices.VFReportQuarantined, Failures: 1, Threshold: 1}, nil
		},
		reportSuccess: func(report devices.VFInitSuccessReport) (devices.VFSuccessResult, error) {
			reports.recordSuccess(report)
			return devices.VFSuccessResult{}, nil
		},
		guestGPUInitStatus: guestReportsFailed,
		initFailures:       counter,
		quarantines:        counter,
		checks:             counter,
	}
	return c, reports
}

func TestVGPUSentinelControllerReportsFailure(t *testing.T) {
	t.Parallel()

	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		assignedAt: "2026-08-20T15:00:00Z",
	}}}
	c, reports := newTestSentinelController(t, store)
	var logs bytes.Buffer
	c.log = slog.New(slog.NewTextHandler(&logs, nil))
	ctx := context.Background()

	c.guestGPUInitStatus = func(context.Context, vgpuSentinelTarget) (guest.GPUInitState, string, error) {
		return guest.GPUInitState_GPU_INIT_STATE_UNKNOWN, "", nil
	}
	c.pollOnce(ctx)
	assert.Empty(t, reports.failures, "an undecided init must not be reported")

	c.guestGPUInitStatus = guestReportsFailed
	c.pollOnce(ctx)
	require.Len(t, reports.failures, 1)
	assert.Equal(t, "0000:e3:00.4", reports.failures[0].VFAddress)
	assert.Equal(t, "instance-1", reports.failures[0].InstanceID)
	assert.Contains(t, logs.String(), "quarantined wedged vGPU VF")
	assert.Contains(t, logs.String(), "RmInitAdapter failed!")
}

func TestVGPUSentinelControllerSkipsUnreachableGuest(t *testing.T) {
	t.Parallel()

	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		assignedAt: "2026-08-20T15:00:00Z",
	}}}
	c, reports := newTestSentinelController(t, store)
	c.guestGPUInitStatus = func(context.Context, vgpuSentinelTarget) (guest.GPUInitState, string, error) {
		return guest.GPUInitState_GPU_INIT_STATE_UNKNOWN, "", errors.New("vsock dial failed")
	}

	c.pollOnce(context.Background())
	assert.Empty(t, reports.failures)
	assert.Empty(t, reports.successes)
}

func TestVGPUSentinelControllerRepairsHealthStoreOncePerPoll(t *testing.T) {
	t.Parallel()

	targets := []vgpuSentinelTarget{{instanceID: "instance-1"}, {instanceID: "instance-2"}}
	c, _ := newTestSentinelController(t, &fakeSentinelStore{targets: targets})
	c.guestGPUInitStatus = guestReportsOK
	var repairs int
	c.repairHealthStore = func() error {
		repairs++
		return errors.New("persist failed")
	}

	c.pollOnce(context.Background())
	assert.Equal(t, 1, repairs)
}

func TestVGPUSentinelControllerRecordsCheckResults(t *testing.T) {
	t.Parallel()

	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{
		{instanceID: "ok"},
		{instanceID: "unknown"},
		{instanceID: "unreachable"},
		{instanceID: "unsupported"},
	}}
	c, _ := newTestSentinelController(t, store)
	reader := otelmetric.NewManualReader()
	provider := otelmetric.NewMeterProvider(otelmetric.WithReader(reader))
	checks, err := provider.Meter("test").Int64Counter("hypeman_instances_vgpu_sentinel_checks_total")
	require.NoError(t, err)
	c.checks = checks
	c.guestGPUInitStatus = func(_ context.Context, target vgpuSentinelTarget) (guest.GPUInitState, string, error) {
		switch target.instanceID {
		case "ok":
			return guest.GPUInitState_GPU_INIT_STATE_OK, "", nil
		case "unknown":
			return guest.GPUInitState_GPU_INIT_STATE_UNKNOWN, "", nil
		case "unsupported":
			return guest.GPUInitState_GPU_INIT_STATE_UNKNOWN, "", fmt.Errorf("gpu init status RPC: %w", status.Error(codes.Unimplemented, "method not implemented"))
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
	assert.Equal(t, map[string]int64{"ok": 1, "unknown": 1, "rpc_error": 1, "unsupported_agent": 1}, got)
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
			c, reports := newTestSentinelController(t, store)
			c.reportFailure = func(report devices.VFInitFailureReport) (devices.VFReportResult, error) {
				reports.recordFailure(report)
				return tt.failure, nil
			}
			c.reportSuccess = func(report devices.VFInitSuccessReport) (devices.VFSuccessResult, error) {
				reports.recordSuccess(report)
				return tt.success, nil
			}

			c.pollOnce(context.Background())
			require.Len(t, reports.failures, 1)
			require.Empty(t, reports.successes)

			// A wedge that recovers (e.g. driver reload) flips the guest state.
			c.guestGPUInitStatus = guestReportsOK
			c.pollOnce(context.Background())
			require.Len(t, reports.failures, 1)
			require.Len(t, reports.successes, 1)
			assert.Equal(t, "instance-1", reports.successes[0].InstanceID)
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
	c, reports := newTestSentinelController(t, store)
	c.guestGPUInitStatus = guestReportsOK
	c.reportSuccess = func(report devices.VFInitSuccessReport) (devices.VFSuccessResult, error) {
		if reports.recordSuccess(report) == 1 {
			return devices.VFSuccessResult{}, errors.New("persist failed")
		}
		return devices.VFSuccessResult{Cleared: 1}, nil
	}

	c.pollOnce(context.Background())
	require.Len(t, reports.successes, 1)

	c.pollOnce(context.Background())
	require.Len(t, reports.successes, 2)
	assert.Equal(t, "0000:e3:00.4", reports.successes[1].VFAddress)
}

func TestVGPUSentinelControllerRetriesFailedQuarantine(t *testing.T) {
	t.Parallel()

	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		assignedAt: "2026-08-20T15:00:00Z",
	}}}
	c, reports := newTestSentinelController(t, store)
	realReport := c.reportFailure
	c.reportFailure = func(devices.VFInitFailureReport) (devices.VFReportResult, error) {
		return devices.VFReportResult{}, errors.New("persist failed")
	}

	c.pollOnce(context.Background())
	assert.Empty(t, reports.failures)

	c.reportFailure = realReport
	c.pollOnce(context.Background())
	assert.Len(t, reports.failures, 1)
}

func TestVGPUSentinelControllerConfirmsAssignmentBeforeReporting(t *testing.T) {
	t.Parallel()

	stale := vgpuSentinelTarget{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		assignedAt: "2026-08-21T00:00:00Z",
	}
	changed := []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.5",
		assignedAt: "2026-08-21T00:00:10Z",
	}}
	tests := []struct {
		name          string
		guestState    func(context.Context, vgpuSentinelTarget) (guest.GPUInitState, string, error)
		targets       []vgpuSentinelTarget
		wantFailures  int
		wantSuccesses int
	}{
		{name: "failure skipped when the assignment changed", guestState: guestReportsFailed, targets: changed},
		// A released assignment (instance gone) remains attributable.
		{name: "failure from a released assignment is reported", guestState: guestReportsFailed, wantFailures: 1},
		{name: "init OK skipped when the assignment changed", guestState: guestReportsOK, targets: changed},
		{name: "init OK from a released assignment clears", guestState: guestReportsOK, wantSuccesses: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, reports := newTestSentinelController(t, &fakeSentinelStore{targets: tt.targets})
			c.guestGPUInitStatus = tt.guestState

			c.pollTarget(context.Background(), stale)

			require.Len(t, reports.failures, tt.wantFailures)
			require.Len(t, reports.successes, tt.wantSuccesses)
			for _, report := range reports.failures {
				assert.Equal(t, stale.vfAddress, report.VFAddress)
				assert.Equal(t, stale.assignedAt, report.AssignedAt)
			}
			for _, report := range reports.successes {
				assert.Equal(t, stale.vfAddress, report.VFAddress)
				assert.Equal(t, stale.assignedAt, report.AssignedAt)
			}
		})
	}
}

func TestGetVGPUSentinelTargetMapsClaimAndRequiresControlSocket(t *testing.T) {
	m := &manager{paths: paths.New(t.TempDir())}
	const instanceID = "stopped-vgpu"
	claimed := time.Now().UTC()
	socketPath := testSentinelSocket(t, instanceID)
	require.NoError(t, m.ensureDirectories(instanceID))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:             instanceID,
		HypervisorType: "cloud-hypervisor",
		VsockSocket:    "/run/vsock.sock",
		VsockCID:       42,
		GPUFramework:   devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath:  "/sys/bus/pci/devices/0000:e3:00.4",
		GPUClaimedAt:   &claimed,
		SocketPath:     socketPath,
	}}))

	target, ok, err := m.getVGPUSentinelTarget(context.Background(), instanceID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, vgpuSentinelTarget{
		instanceID:     instanceID,
		vfAddress:      "0000:e3:00.4",
		assignedAt:     claimed.Format(time.RFC3339Nano),
		hypervisorType: "cloud-hypervisor",
		vsockSocket:    "/run/vsock.sock",
		vsockCID:       42,
	}, target)

	// A stopped or standby instance has no control socket; its lingering
	// claim must not be polled.
	require.NoError(t, os.Remove(socketPath))
	_, ok, err = m.getVGPUSentinelTarget(context.Background(), instanceID)
	require.NoError(t, err)
	assert.False(t, ok)
}

// testSentinelSocket creates a stand-in control socket file for instanceID.
func testSentinelSocket(t *testing.T, instanceID string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), instanceID+".sock")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	return path
}

func TestListVGPUSentinelTargetsSkipsUnstattableMetadata(t *testing.T) {
	m := &manager{paths: paths.New(t.TempDir())}
	claimed := time.Now().UTC()
	require.NoError(t, m.ensureDirectories("readable"))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:            "readable",
		GPUFramework:  devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath: "/sys/bus/pci/devices/0000:e3:00.4",
		GPUClaimedAt:  &claimed,
		SocketPath:    testSentinelSocket(t, "readable"),
	}}))
	// A self-referential symlink makes stat fail with ELOOP rather than
	// ENOENT, and does so for root too.
	for _, id := range []string{"unreadable-a", "unreadable-b"} {
		require.NoError(t, m.ensureDirectories(id))
		metaPath := m.paths.InstanceMetadata(id)
		require.NoError(t, os.Symlink(filepath.Base(metaPath), metaPath))
	}

	files, err := m.listMetadataFilesStrict()
	require.Error(t, err)
	assert.ErrorContains(t, err, "unreadable-a")
	assert.ErrorContains(t, err, "unreadable-b")
	assert.Nil(t, files)

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
			wantScans:   0,
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
