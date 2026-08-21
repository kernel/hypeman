package instances

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"
)

const testSentinelLine = "2026/08/20 15:04:05 [guest-agent] HYPEMAN-GPU-INIT-FAILED ts=2026-08-20T15:04:05.123456789Z nvrm=\"NVRM: GPU 0000:e3:00.4: RmInitAdapter failed! (0x22:0x65:884)\"\n"

func TestVGPUSentinelPattern(t *testing.T) {
	t.Parallel()

	assert.True(t, vgpuSentinelPattern.MatchString(testSentinelLine))

	// The raw guest kernel line is not the conviction signal: the guest
	// agent observes it in the guest and reports the marker instead.
	assert.False(t, vgpuSentinelPattern.MatchString("[   27.031415] NVRM: GPU 0000:e3:00.4: RmInitAdapter failed! (0x22:0x65:884)"))

	// Echoed exec command lines can carry the bare token, a truncated marker,
	// or a marker-shaped line without the NVRM payload on the same console.
	assert.False(t, vgpuSentinelPattern.MatchString("$ dmesg | grep -c HYPEMAN-GPU-INIT-FAILED"))
	assert.False(t, vgpuSentinelPattern.MatchString("$ echo 'HYPEMAN-GPU-INIT-FAILED ts=x nvrm=\"'"))
	assert.False(t, vgpuSentinelPattern.MatchString("HYPEMAN-GPU-INIT-FAILED ts=2026-08-20T15:04:05Z nvrm=\"something else\""))
	assert.False(t, vgpuSentinelPattern.MatchString("2026/08/20 15:04:05 [guest-agent] HYPEMAN-AGENT-READY ts=2026-08-20T15:04:05Z"))
}

type fakeSentinelStore struct {
	targets []vgpuSentinelTarget
}

func (s *fakeSentinelStore) listVGPUSentinelTargets(context.Context) ([]vgpuSentinelTarget, error) {
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

func newTestSentinelController(t *testing.T, store vgpuSentinelStore) (*VGPUSentinelController, *[]devices.VFQuarantine) {
	t.Helper()
	counter, err := noop.NewMeterProvider().Meter("test").Int64Counter("test")
	require.NoError(t, err)
	var quarantined []devices.VFQuarantine
	c := &VGPUSentinelController{
		store:    store,
		log:      slog.New(slog.DiscardHandler),
		interval: time.Hour,
		quarantine: func(q devices.VFQuarantine) (bool, error) {
			quarantined = append(quarantined, q)
			return false, nil
		},
		convictions: counter,
		tails:       make(map[string]*vgpuSentinelTail),
	}
	return c, &quarantined
}

func TestVGPUSentinelControllerConvictsOnce(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		appLogPath: logPath,
	}}}
	c, quarantined := newTestSentinelController(t, store)
	ctx := context.Background()

	// Log missing (boot not started) and then healthy output: no conviction.
	c.scanOnce(ctx)
	require.NoError(t, os.WriteFile(logPath, []byte("booting\nnvidia driver loaded\n"), 0644))
	c.scanOnce(ctx)
	assert.Empty(t, *quarantined)

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
	require.NoError(t, err)
	// A partial line without its newline must not convict yet.
	_, err = f.WriteString("2026/08/20 15:04:05 [guest-agent] HYPEMAN-GPU-INIT-FAILED ts=2026-08-20T15:04:05Z nvrm=\"NVRM: GPU 0000:e3:00.4: RmInitAdapter fail")
	require.NoError(t, err)
	c.scanOnce(ctx)
	assert.Empty(t, *quarantined)

	_, err = f.WriteString("ed! (0x22:0x65:884)\"\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	c.scanOnce(ctx)
	require.Len(t, *quarantined, 1)
	assert.Equal(t, "0000:e3:00.4", (*quarantined)[0].VFAddress)
	assert.Equal(t, "instance-1", (*quarantined)[0].InstanceID)
	assert.Contains(t, (*quarantined)[0].SentinelLine, "RmInitAdapter failed!")

	// The guest agent re-emits the marker while the failure persists; a
	// convicted instance is not re-convicted.
	appendSentinelLine(t, logPath)
	c.scanOnce(ctx)
	assert.Len(t, *quarantined, 1)
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
	c, quarantined := newTestSentinelController(t, store)
	quarantineErr := errors.New("persist failed")
	realQuarantine := c.quarantine
	c.quarantine = func(devices.VFQuarantine) (bool, error) {
		return false, quarantineErr
	}
	ctx := context.Background()

	c.scanOnce(ctx)
	assert.Empty(t, *quarantined)
	assert.False(t, c.tails["instance-1"].done)

	// The next recurrence of the marker retries the quarantine.
	c.quarantine = realQuarantine
	appendSentinelLine(t, logPath)
	c.scanOnce(ctx)
	assert.Len(t, *quarantined, 1)
	assert.True(t, c.tails["instance-1"].done)
}

// A burst of convictions across many instances is quarantined without any
// rate limit: systemic non-wedge failures (e.g. a driver-mismatch rollout)
// are expected to be caught on a test host before reaching production, and
// the convictions counter is the alerting signal if one gets through.
func TestVGPUSentinelControllerConvictsBursts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	targets := make([]vgpuSentinelTarget, 0, 5)
	for i := 0; i < 5; i++ {
		logPath := filepath.Join(dir, string(rune('a'+i))+".log")
		require.NoError(t, os.WriteFile(logPath, []byte(testSentinelLine), 0644))
		targets = append(targets, vgpuSentinelTarget{
			instanceID: "instance-" + string(rune('a'+i)),
			vfAddress:  "0000:e3:00." + string(rune('4'+i)),
			appLogPath: logPath,
		})
	}
	c, quarantined := newTestSentinelController(t, &fakeSentinelStore{targets: targets})

	c.scanOnce(context.Background())
	assert.Len(t, *quarantined, len(targets))
	for _, target := range targets {
		assert.True(t, c.tails[target.instanceID].done)
	}
}

func TestVGPUSentinelControllerSkipsQuarantinedVFs(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	require.NoError(t, os.WriteFile(logPath, []byte(testSentinelLine), 0644))
	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		appLogPath: logPath,
	}}}
	c, _ := newTestSentinelController(t, store)
	c.quarantine = func(devices.VFQuarantine) (bool, error) {
		return true, nil
	}

	// A rescan of a standing victim's log after a controller restart reports
	// an existing quarantine: not a new wedge, and the tail closes.
	c.scanOnce(context.Background())
	assert.True(t, c.tails["instance-1"].done)
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
	c, quarantined := newTestSentinelController(t, store)
	ctx := context.Background()

	c.scanOnce(ctx)
	require.Len(t, *quarantined, 1)
	require.True(t, c.tails["instance-1"].done)

	// A stop/start acquires a new assignment (possibly a different VF) and
	// archives the log; the finished tail from the previous assignment must
	// not suppress scanning the new boot.
	require.NoError(t, os.WriteFile(logPath, []byte(testSentinelLine), 0644))
	store.targets[0].vfAddress = "0000:e3:00.5"
	store.targets[0].assignedAt = "2026-08-20T16:00:00Z"
	c.scanOnce(ctx)
	require.Len(t, *quarantined, 2)
	assert.Equal(t, "0000:e3:00.5", (*quarantined)[1].VFAddress)
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
	ctx := context.Background()

	c.scanOnce(ctx)
	require.Contains(t, c.tails, "instance-1")

	store.targets = nil
	c.scanOnce(ctx)
	assert.NotContains(t, c.tails, "instance-1")
}

// A marker read against a metadata snapshot must not convict once the
// instance's assignment has changed under the scan: the marker belonged to
// the old epoch's VF, and the current VF may be healthy.
func TestVGPUSentinelControllerSkipsConvictionOnChangedAssignment(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	require.NoError(t, os.WriteFile(logPath, []byte(testSentinelLine), 0644))
	store := &fakeSentinelStore{targets: []vgpuSentinelTarget{{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.5",
		appLogPath: logPath,
		assignedAt: "2026-08-21T00:00:10Z",
	}}}
	c, quarantined := newTestSentinelController(t, store)

	stale := vgpuSentinelTarget{
		instanceID: "instance-1",
		vfAddress:  "0000:e3:00.4",
		appLogPath: logPath,
		assignedAt: "2026-08-21T00:00:00Z",
	}
	c.scanTarget(context.Background(), stale)
	assert.Empty(t, *quarantined)
	assert.False(t, c.tails["instance-1"].done)

	// A stop or delete after the marker was read releases the assignment but
	// cannot un-wedge the VF: the marker still convicts the scanned epoch's VF.
	store.targets = nil
	c.tails["instance-1"] = &vgpuSentinelTail{vfAddress: stale.vfAddress, assignedAt: stale.assignedAt}
	c.scanTarget(context.Background(), stale)
	require.Len(t, *quarantined, 1)
	assert.Equal(t, "0000:e3:00.4", (*quarantined)[0].VFAddress)
}

func TestListVGPUSentinelTargetsSkipsUnstattableMetadata(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	m := &manager{paths: paths.New(t.TempDir())}
	assigned := time.Now().UTC()
	for _, id := range []string{"readable", "unreadable"} {
		require.NoError(t, m.ensureDirectories(id))
		require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
			Id:            id,
			GPUFramework:  devices.VGPUFrameworkVendorVFIO,
			GPUDevicePath: "/sys/bus/pci/devices/0000:e3:00.4",
			GPUAssignedAt: &assigned,
		}}))
	}
	instanceDir := filepath.Dir(m.paths.InstanceMetadata("unreadable"))
	require.NoError(t, os.Chmod(instanceDir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(instanceDir, 0o755) })

	targets, err := m.listVGPUSentinelTargets(context.Background())
	require.NoError(t, err)
	require.Len(t, targets, 1)
	assert.Equal(t, "readable", targets[0].instanceID)
}

// A transient discovery error fails open — the controller scans as if the
// host were vendor VFIO — but must keep re-probing so a host that is not
// vendor VFIO stops scanning once discovery recovers.
func TestVGPUSentinelControllerRunReprobesFailedDiscovery(t *testing.T) {
	t.Parallel()

	c, _ := newTestSentinelController(t, &fakeSentinelStore{})
	c.interval = time.Millisecond
	var probes int
	c.discoverFramework = func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
		probes++
		if probes == 1 {
			return devices.VGPUFrameworkNone, nil, errors.New("transient sysfs error")
		}
		return devices.VGPUFrameworkNone, nil, nil
	}

	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not exit after discovery resolved to a non vendor VFIO host")
	}
	assert.Equal(t, 2, probes)
}

// Kernel printk shares the serial console with the agent and can split a
// marker mid-write — which is why the agent emits each report as several
// identical lines. A corrupted copy must not convict (the strict shape is
// what keeps echoed commands from convicting), and the intact repeat on the
// next line must.
func TestScanForSentinelConvictsOnIntactRepeatAfterSplitMarker(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	split := "2026/08/20 15:04:05 [guest-agent] HYPEMAN-GPU-INIT-FAILED ts=2026-08-20T15:04:05Z nvrm=\"NVRM: GPU 0000:e3:0\n" +
		"[   27.031415] NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)\n" +
		"0.4: RmInitAdapter failed! (0x22:0x65:884)\"\n"
	require.NoError(t, os.WriteFile(logPath, []byte(split), 0644))

	tail := &vgpuSentinelTail{}
	_, found, err := scanForSentinel(logPath, tail)
	require.NoError(t, err)
	assert.False(t, found, "neither a split marker nor the raw kernel line may convict")

	appendSentinelLine(t, logPath)
	line, found, err := scanForSentinel(logPath, tail)
	require.NoError(t, err)
	require.True(t, found, "the intact repeat must convict")
	assert.Contains(t, line, "HYPEMAN-GPU-INIT-FAILED")
}

// The marker is a few hundred bytes, so a line that overflows the read
// buffer is guest console spam by definition: it must not convict even when
// it embeds a marker, must never be buffered whole, and its tail — arriving
// on a later scan — must not be parsed as a fresh line.
func TestScanForSentinelDiscardsOversizedLines(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	tail := &vgpuSentinelTail{}

	// An oversized terminated line with an embedded marker: skipped whole.
	huge := strings.Repeat("x", vgpuSentinelMaxLineBytes) + testSentinelLine
	require.NoError(t, os.WriteFile(logPath, []byte(huge), 0644))
	line, found, err := scanForSentinel(logPath, tail)
	require.NoError(t, err)
	assert.False(t, found, "a marker inside an oversized line must not convict")
	assert.Empty(t, line)

	// A short marker line after the oversized one still convicts.
	appendSentinelLine(t, logPath)
	line, found, err = scanForSentinel(logPath, tail)
	require.NoError(t, err)
	require.True(t, found)
	assert.Contains(t, line, "RmInitAdapter failed!")
}

func TestScanForSentinelDiscardsOversizedLineTailAcrossScans(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "app.log")
	tail := &vgpuSentinelTail{}

	// An oversized line still missing its newline: the scan enters skip mode.
	require.NoError(t, os.WriteFile(logPath, []byte(strings.Repeat("x", vgpuSentinelMaxLineBytes+10)), 0644))
	_, found, err := scanForSentinel(logPath, tail)
	require.NoError(t, err)
	assert.False(t, found)
	assert.True(t, tail.skippingLongLine)

	// The line's tail arrives later carrying a marker shape; it is still the
	// same oversized line, so it must be discarded, not parsed as fresh.
	appendSentinelLine(t, logPath)
	_, found, err = scanForSentinel(logPath, tail)
	require.NoError(t, err)
	assert.False(t, found, "the tail of an oversized line must not convict")
	assert.False(t, tail.skippingLongLine)

	// The next genuine marker line convicts.
	appendSentinelLine(t, logPath)
	line, found, err := scanForSentinel(logPath, tail)
	require.NoError(t, err)
	require.True(t, found)
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

	// Rotation truncates the file under the tail; the restart from the top
	// must clear the skip state or a marker in the fresh log would be lost.
	require.NoError(t, os.WriteFile(logPath, []byte(testSentinelLine), 0644))
	line, found, err := scanForSentinel(logPath, tail)
	require.NoError(t, err)
	require.True(t, found)
	assert.Contains(t, line, "RmInitAdapter failed!")
	assert.False(t, tail.skippingLongLine)
}

func appendSentinelLine(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	require.NoError(t, err)
	_, err = f.WriteString(testSentinelLine)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}
