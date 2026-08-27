package instances

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"
)

const testSentinelMarker = "HYPEMAN-GPU-INIT-FAILED ts=2026-08-20T15:04:05.123456789Z nvrm=\"NVRM: GPU 0000:e3:00.4: RmInitAdapter failed! (0x22:0x65:884)\""
const testSentinelLine = "2026/08/20 15:04:05 [guest-agent] " + testSentinelMarker + "\n"
const testSentinelOKMarker = "HYPEMAN-GPU-INIT-OK ts=2026-08-20T15:04:05.123456789Z"
const testSentinelOKLine = "2026/08/20 15:04:05 [guest-agent] " + testSentinelOKMarker + "\n"

func TestVGPUSentinelPattern(t *testing.T) {
	t.Parallel()

	marker, event := MatchVGPUSentinelLine([]byte(testSentinelLine))
	assert.Equal(t, VGPUSentinelEventInitFailed, event)
	assert.Equal(t, testSentinelMarker, marker)

	marker, event = MatchVGPUSentinelLine([]byte(testSentinelOKLine))
	assert.Equal(t, VGPUSentinelEventInitOK, event)
	assert.Equal(t, testSentinelOKMarker, marker)

	for _, line := range []string{
		"[   27.031415] NVRM: GPU 0000:e3:00.4: RmInitAdapter failed! (0x22:0x65:884)",
		testSentinelMarker + "\n",
		"customer output: " + testSentinelOKMarker + "\n",
		"2026-08-20T15:04:05Z [INFO] [hypeman-init:entrypoint] cmd=[echo " + testSentinelMarker + "]\n",
		strings.TrimSuffix(testSentinelLine, "\n") + " trailing customer output\n",
		"2026/08/20 15:04:05 [guest-agent] HYPEMAN-GPU-INIT-FAILED ts=2026-08-20T15:04:05Z nvrm=\"something else\"\n",
		"2026/08/20 15:04:05 [guest-agent] HYPEMAN-AGENT-READY ts=2026-08-20T15:04:05Z",
	} {
		_, event := MatchVGPUSentinelLine([]byte(line))
		assert.Equal(t, VGPUSentinelEventNone, event, "must not match %q", line)
	}
}

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

func newTestSentinelController(t *testing.T, store vgpuSentinelStore) (*VGPUSentinelController, *[]devices.VFInitFailureReport) {
	t.Helper()
	counter, err := noop.NewMeterProvider().Meter("test").Int64Counter("test")
	require.NoError(t, err)
	var reported []devices.VFInitFailureReport
	c := &VGPUSentinelController{
		store:    store,
		log:      slog.New(slog.DiscardHandler),
		interval: time.Hour,
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
		// Failure markers corroborate by default; success-path tests override.
		guestGPUInitState: func(context.Context, string) (guest.GPUInitState, error) {
			return guest.GPUInitState_GPU_INIT_STATE_FAILED, nil
		},
		initFailures: counter,
		quarantines:  counter,
		tails:        make(map[string]*vgpuSentinelTail),
	}
	return c, &reported
}

func corroborateOK(context.Context, string) (guest.GPUInitState, error) {
	return guest.GPUInitState_GPU_INIT_STATE_OK, nil
}

func TestVGPUSentinelControllerReportsFailureOnce(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		appLogPath: logPath,
	}}}
	c, reported := newTestSentinelController(t, store)
	ctx := context.Background()

	c.scanOnce(ctx)
	require.NoError(t, os.WriteFile(logPath, []byte("booting\nnvidia driver loaded\n"), 0644))
	c.scanOnce(ctx)
	assert.Empty(t, *reported)

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
	require.NoError(t, err)
	_, err = f.WriteString("2026/08/20 15:04:05 [guest-agent] HYPEMAN-GPU-INIT-FAILED ts=2026-08-20T15:04:05Z nvrm=\"NVRM: GPU 0000:e3:00.4: RmInitAdapter fail")
	require.NoError(t, err)
	c.scanOnce(ctx)
	assert.Empty(t, *reported)

	_, err = f.WriteString("ed! (0x22:0x65:884)\"\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	c.scanOnce(ctx)
	require.Len(t, *reported, 1)
	assert.Equal(t, "0000:e3:00.4", (*reported)[0].VFAddress)
	assert.Equal(t, "instance-1", (*reported)[0].InstanceID)

	appendSentinelLine(t, logPath)
	c.scanOnce(ctx)
	assert.Len(t, *reported, 1)
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
			logPath := filepath.Join(t.TempDir(), "app.log")
			require.NoError(t, os.WriteFile(logPath, []byte(testSentinelLine), 0644))
			store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
				instanceID: "instance-1",
				vfAddress:  "0000:e3:00.4",
				appLogPath: logPath,
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

			c.scanOnce(context.Background())
			require.Len(t, *reported, 1)
			require.Empty(t, successes)

			c.guestGPUInitState = corroborateOK
			f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
			require.NoError(t, err)
			_, err = f.WriteString(testSentinelOKLine)
			require.NoError(t, err)
			require.NoError(t, f.Close())
			c.scanOnce(context.Background())
			require.Len(t, *reported, 1)
			require.Len(t, successes, 1)
			assert.Equal(t, "instance-1", successes[0].InstanceID)
		})
	}
}

func TestVGPUSentinelControllerSkipsReplayedResolvedPair(t *testing.T) {
	t.Parallel()

	// A restart rescans the log from offset zero; a FAILED marker that was
	// already resolved by a later OK must not be recorded again.
	logPath := filepath.Join(t.TempDir(), "app.log")
	require.NoError(t, os.WriteFile(logPath, []byte(testSentinelLine+testSentinelOKLine), 0644))
	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		appLogPath: logPath,
		assignedAt: "2026-08-20T15:00:00Z",
	}}}
	c, reported := newTestSentinelController(t, store)
	c.guestGPUInitState = corroborateOK
	var successes []devices.VFInitSuccessReport
	c.reportSuccess = func(report devices.VFInitSuccessReport) (devices.VFSuccessResult, error) {
		successes = append(successes, report)
		return devices.VFSuccessResult{}, nil
	}

	c.scanOnce(context.Background())
	assert.Empty(t, *reported)
	require.Len(t, successes, 1)
}

func TestVGPUSentinelControllerIgnoresUncorroboratedMarkers(t *testing.T) {
	tests := []struct {
		name       string
		line       string
		guestState func(context.Context, string) (guest.GPUInitState, error)
	}{
		{
			name: "forged failure marker",
			line: testSentinelLine,
			guestState: func(context.Context, string) (guest.GPUInitState, error) {
				return guest.GPUInitState_GPU_INIT_STATE_UNKNOWN, nil
			},
		},
		{
			name: "unreachable guest agent",
			line: testSentinelLine,
			guestState: func(context.Context, string) (guest.GPUInitState, error) {
				return guest.GPUInitState_GPU_INIT_STATE_UNKNOWN, errors.New("vsock dial failed")
			},
		},
		{
			name: "forged success marker",
			line: testSentinelOKLine,
			guestState: func(context.Context, string) (guest.GPUInitState, error) {
				return guest.GPUInitState_GPU_INIT_STATE_UNKNOWN, nil
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "app.log")
			require.NoError(t, os.WriteFile(logPath, []byte(tt.line), 0644))
			store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
				instanceID: "instance-1",
				vfAddress:  "0000:e3:00.4",
				appLogPath: logPath,
				assignedAt: "2026-08-20T15:00:00Z",
			}}}
			c, reported := newTestSentinelController(t, store)
			var logs bytes.Buffer
			c.log = slog.New(slog.NewTextHandler(&logs, nil))
			c.guestGPUInitState = tt.guestState
			var successes []devices.VFInitSuccessReport
			c.reportSuccess = func(report devices.VFInitSuccessReport) (devices.VFSuccessResult, error) {
				successes = append(successes, report)
				return devices.VFSuccessResult{}, nil
			}

			c.scanOnce(context.Background())
			assert.Empty(t, *reported)
			assert.Empty(t, successes)
			assert.Contains(t, logs.String(), "corroborate")
		})
	}
}

func TestVGPUSentinelControllerRetriesFailedTallyClear(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	require.NoError(t, os.WriteFile(logPath, []byte(testSentinelOKLine), 0644))
	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		appLogPath: logPath,
		assignedAt: "2026-08-20T15:00:00Z",
	}}}
	c, _ := newTestSentinelController(t, store)
	c.guestGPUInitState = corroborateOK
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

	c.scanOnce(context.Background())
	require.Len(t, reports, 1)
	assert.Contains(t, logs.String(), "persist failed")

	// The once-per-boot OK marker is already consumed; the clear must be
	// retried without a new marker.
	c.scanOnce(context.Background())
	require.Len(t, reports, 2)
	assert.Equal(t, "0000:e3:00.4", reports[1].VFAddress)

	c.scanOnce(context.Background())
	require.Len(t, reports, 2)
}

func TestVGPUSentinelControllerRetriesPendingClearBeforeTailReplacement(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	require.NoError(t, os.WriteFile(logPath, []byte(testSentinelOKLine), 0644))
	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		appLogPath: logPath,
		assignedAt: "2026-08-20T15:00:00Z",
	}}}
	c, _ := newTestSentinelController(t, store)
	c.guestGPUInitState = corroborateOK
	var reports []devices.VFInitSuccessReport
	c.reportSuccess = func(report devices.VFInitSuccessReport) (devices.VFSuccessResult, error) {
		reports = append(reports, report)
		if len(reports) == 1 {
			return devices.VFSuccessResult{}, errors.New("persist failed")
		}
		return devices.VFSuccessResult{Cleared: 1}, nil
	}

	c.scanOnce(context.Background())
	require.Len(t, reports, 1)

	// A stop/start replaces the assignment; the old assignment's clear must be
	// retried before its tail is dropped.
	require.NoError(t, os.WriteFile(logPath, nil, 0644))
	store.targets[0].vfAddress = "0000:e3:00.5"
	store.targets[0].assignedAt = "2026-08-20T16:00:00Z"
	c.scanOnce(context.Background())
	require.Len(t, reports, 2)
	assert.Equal(t, "0000:e3:00.4", reports[1].VFAddress)
	assert.Equal(t, "2026-08-20T15:00:00Z", reports[1].AssignedAt)
	assert.False(t, c.tails["instance-1"].pendingSuccess)
}

func TestVGPUSentinelControllerSkipsInitOKOnChangedAssignment(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	require.NoError(t, os.WriteFile(logPath, []byte(testSentinelOKLine), 0644))
	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.5",
		appLogPath: logPath,
		assignedAt: "2026-08-21T00:00:10Z",
	}}}
	c, _ := newTestSentinelController(t, store)
	var cleared []devices.VFInitSuccessReport
	c.reportSuccess = func(report devices.VFInitSuccessReport) (devices.VFSuccessResult, error) {
		cleared = append(cleared, report)
		return devices.VFSuccessResult{Cleared: 1}, nil
	}

	c.scanTarget(context.Background(), vgpuSentinelTarget{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		appLogPath: logPath,
		assignedAt: "2026-08-21T00:00:00Z",
	})
	assert.Empty(t, cleared)
}

func TestVGPUSentinelControllerAppliesInitOKFromReleasedAssignment(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	require.NoError(t, os.WriteFile(logPath, []byte(testSentinelOKLine), 0644))
	c, _ := newTestSentinelController(t, &fakeSentinelStore{})
	c.guestGPUInitState = corroborateOK
	var cleared []devices.VFInitSuccessReport
	c.reportSuccess = func(report devices.VFInitSuccessReport) (devices.VFSuccessResult, error) {
		cleared = append(cleared, report)
		return devices.VFSuccessResult{Cleared: 1}, nil
	}

	c.scanTarget(context.Background(), vgpuSentinelTarget{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		appLogPath: logPath,
		assignedAt: "2026-08-21T00:00:00Z",
	})
	require.Len(t, cleared, 1)
	assert.Equal(t, "2026-08-21T00:00:00Z", cleared[0].AssignedAt)
}

func TestVGPUSentinelControllerRetriesFailedQuarantine(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	require.NoError(t, os.WriteFile(logPath, []byte(testSentinelLine), 0644))
	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		appLogPath: logPath,
	}}}
	c, reported := newTestSentinelController(t, store)
	realReport := c.reportFailure
	c.reportFailure = func(devices.VFInitFailureReport) (devices.VFReportResult, error) {
		return devices.VFReportResult{}, errors.New("persist failed")
	}

	c.scanOnce(context.Background())
	assert.Empty(t, *reported)

	c.reportFailure = realReport
	appendSentinelLine(t, logPath)
	c.scanOnce(context.Background())
	assert.Len(t, *reported, 1)
}

func TestVGPUSentinelControllerRescansNewAssignment(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	require.NoError(t, os.WriteFile(logPath, []byte(testSentinelLine), 0644))
	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		appLogPath: logPath,
		assignedAt: "2026-08-20T15:00:00Z",
	}}}
	c, reported := newTestSentinelController(t, store)

	c.scanOnce(context.Background())
	require.Len(t, *reported, 1)

	require.NoError(t, os.WriteFile(logPath, []byte(testSentinelLine), 0644))
	store.targets[0].vfAddress = "0000:e3:00.5"
	store.targets[0].assignedAt = "2026-08-20T16:00:00Z"
	c.scanOnce(context.Background())
	require.Len(t, *reported, 2)
	assert.Equal(t, "0000:e3:00.5", (*reported)[1].VFAddress)
}

func TestVGPUSentinelControllerDropsStaleTails(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	require.NoError(t, os.WriteFile(logPath, []byte("booting\n"), 0644))
	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		appLogPath: logPath,
	}}}
	c, _ := newTestSentinelController(t, store)

	c.scanOnce(context.Background())
	require.Contains(t, c.tails, "instance-1")

	store.targets = nil
	c.scanOnce(context.Background())
	assert.NotContains(t, c.tails, "instance-1")
}

func TestVGPUSentinelControllerSkipsFailureOnChangedAssignment(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	require.NoError(t, os.WriteFile(logPath, []byte(testSentinelLine), 0644))
	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.5",
		appLogPath: logPath,
		assignedAt: "2026-08-21T00:00:10Z",
	}}}
	c, reported := newTestSentinelController(t, store)

	stale := vgpuSentinelTarget{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		appLogPath: logPath,
		assignedAt: "2026-08-21T00:00:00Z",
	}
	c.scanTarget(context.Background(), stale)
	assert.Empty(t, *reported)

	store.targets = nil
	c.tails["instance-1"] = &vgpuSentinelTail{vfAddress: stale.vfAddress, assignedAt: stale.assignedAt}
	c.scanTarget(context.Background(), stale)
	require.Len(t, *reported, 1)
	assert.Equal(t, "0000:e3:00.4", (*reported)[0].VFAddress)
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

func scanForSentinel(path string, tail *vgpuSentinelTail) (string, VGPUSentinelEvent, error) {
	var marker string
	var event VGPUSentinelEvent
	err := scanSentinelLog(path, tail, func(found string, foundEvent VGPUSentinelEvent) {
		marker, event = found, foundEvent
	})
	return marker, event, err
}

func TestScanForSentinelMatchesOnIntactRepeatAfterSplitMarker(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	split := "2026/08/20 15:04:05 [guest-agent] HYPEMAN-GPU-INIT-FAILED ts=2026-08-20T15:04:05Z nvrm=\"NVRM: GPU 0000:e3:0\n" +
		"[   27.031415] NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)\n" +
		"0.4: RmInitAdapter failed! (0x22:0x65:884)\"\n"
	require.NoError(t, os.WriteFile(logPath, []byte(split), 0644))

	tail := &vgpuSentinelTail{}
	_, event, err := scanForSentinel(logPath, tail)
	require.NoError(t, err)
	assert.Equal(t, VGPUSentinelEventNone, event, "neither a split marker nor the raw kernel line may match")

	appendSentinelLine(t, logPath)
	line, event, err := scanForSentinel(logPath, tail)
	require.NoError(t, err)
	require.Equal(t, VGPUSentinelEventInitFailed, event, "the intact repeat must match")
	assert.Contains(t, line, "HYPEMAN-GPU-INIT-FAILED")
}

func TestScanForSentinelDiscardsOversizedLines(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	tail := &vgpuSentinelTail{}

	huge := strings.Repeat("x", vgpuSentinelMaxLineBytes) + testSentinelLine
	require.NoError(t, os.WriteFile(logPath, []byte(huge), 0644))
	line, event, err := scanForSentinel(logPath, tail)
	require.NoError(t, err)
	assert.Equal(t, VGPUSentinelEventNone, event, "a marker inside an oversized line must not match")
	assert.Empty(t, line)

	appendSentinelLine(t, logPath)
	line, event, err = scanForSentinel(logPath, tail)
	require.NoError(t, err)
	require.Equal(t, VGPUSentinelEventInitFailed, event)
	assert.Contains(t, line, "RmInitAdapter failed!")
}

func TestScanForSentinelDiscardsOversizedLineTailAcrossScans(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	tail := &vgpuSentinelTail{}

	require.NoError(t, os.WriteFile(logPath, []byte(strings.Repeat("x", vgpuSentinelMaxLineBytes+10)), 0644))
	_, event, err := scanForSentinel(logPath, tail)
	require.NoError(t, err)
	assert.Equal(t, VGPUSentinelEventNone, event)
	assert.True(t, tail.skippingLongLine)

	appendSentinelLine(t, logPath)
	_, event, err = scanForSentinel(logPath, tail)
	require.NoError(t, err)
	assert.Equal(t, VGPUSentinelEventNone, event, "the tail of an oversized line must not match")
	assert.False(t, tail.skippingLongLine)

	appendSentinelLine(t, logPath)
	line, event, err := scanForSentinel(logPath, tail)
	require.NoError(t, err)
	require.Equal(t, VGPUSentinelEventInitFailed, event)
	assert.Contains(t, line, "RmInitAdapter failed!")
}

func TestScanForSentinelResetsSkipStateOnTruncatedLog(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	tail := &vgpuSentinelTail{}

	require.NoError(t, os.WriteFile(logPath, []byte(strings.Repeat("x", vgpuSentinelMaxLineBytes+10)), 0644))
	_, _, err := scanForSentinel(logPath, tail)
	require.NoError(t, err)
	require.True(t, tail.skippingLongLine)

	require.NoError(t, os.WriteFile(logPath, []byte(testSentinelLine), 0644))
	line, event, err := scanForSentinel(logPath, tail)
	require.NoError(t, err)
	require.Equal(t, VGPUSentinelEventInitFailed, event)
	assert.Contains(t, line, "RmInitAdapter failed!")
	assert.False(t, tail.skippingLongLine)
}

func TestScanForSentinelScansRotatedBackupAfterCopyTruncate(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	tail := &vgpuSentinelTail{}

	require.NoError(t, os.WriteFile(logPath, []byte("booting\n"), 0644))
	_, event, err := scanForSentinel(logPath, tail)
	require.NoError(t, err)
	require.Equal(t, VGPUSentinelEventNone, event)

	// A marker lands and rotation copies it to the .1 backup before the
	// sentinel reads it.
	appendSentinelLine(t, logPath)
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(logPath+".1", data, 0644))
	require.NoError(t, os.Truncate(logPath, 0))

	line, event, err := scanForSentinel(logPath, tail)
	require.NoError(t, err)
	require.Equal(t, VGPUSentinelEventInitFailed, event)
	assert.Contains(t, line, "RmInitAdapter failed!")

	// The offset restarts at the head of the truncated active file.
	require.NoError(t, os.WriteFile(logPath, []byte(testSentinelOKLine), 0644))
	_, event, err = scanForSentinel(logPath, tail)
	require.NoError(t, err)
	assert.Equal(t, VGPUSentinelEventInitOK, event)
}

func appendSentinelLine(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	require.NoError(t, err)
	_, err = f.WriteString(testSentinelLine)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}
