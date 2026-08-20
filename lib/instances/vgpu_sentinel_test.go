package instances

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
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

func newTestSentinelController(t *testing.T, store vgpuSentinelStore) (*VGPUSentinelController, *[]devices.VFQuarantine) {
	t.Helper()
	counter, err := noop.NewMeterProvider().Meter("test").Int64Counter("test")
	require.NoError(t, err)
	var quarantined []devices.VFQuarantine
	c := &VGPUSentinelController{
		store:    store,
		log:      slog.New(slog.DiscardHandler),
		interval: time.Hour,
		now:      time.Now,
		quarantine: func(q devices.VFQuarantine) (devices.VFHealthRecord, bool, error) {
			quarantined = append(quarantined, q)
			return devices.VFHealthRecord{VFAddress: q.VFAddress, WedgeCount: 1}, false, nil
		},
		isQuarantined: func(string) bool { return false },
		convictions:   counter,
		tails:         make(map[string]*vgpuSentinelTail),
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
	c.quarantine = func(devices.VFQuarantine) (devices.VFHealthRecord, bool, error) {
		return devices.VFHealthRecord{}, false, quarantineErr
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

func TestVGPUSentinelControllerBrakePausesConvictionBursts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	targets := make([]vgpuSentinelTarget, 0, vgpuSentinelBrakeLimit+1)
	for i := 0; i < vgpuSentinelBrakeLimit+1; i++ {
		logPath := filepath.Join(dir, string(rune('a'+i))+".log")
		require.NoError(t, os.WriteFile(logPath, []byte(testSentinelLine), 0644))
		targets = append(targets, vgpuSentinelTarget{
			instanceID: "instance-" + string(rune('a'+i)),
			vfAddress:  "0000:e3:00." + string(rune('4'+i)),
			appLogPath: logPath,
		})
	}
	c, quarantined := newTestSentinelController(t, &fakeSentinelStore{targets: targets})
	base := time.Now()
	c.now = func() time.Time { return base }

	c.scanOnce(context.Background())
	// The burst converts to convictions up to the limit; the rest are
	// suppressed rather than quarantining the whole host, and their tails
	// stay open — the brake pauses conviction, it does not drop it.
	assert.Len(t, *quarantined, vgpuSentinelBrakeLimit)
	suppressed := 0
	for _, target := range targets {
		if !c.tails[target.instanceID].done {
			suppressed++
			// The guest agent re-emits the marker while the failure persists.
			appendSentinelLine(t, target.appLogPath)
		}
	}
	assert.Equal(t, 1, suppressed)

	// Within the window the suppression holds.
	c.scanOnce(context.Background())
	assert.Len(t, *quarantined, vgpuSentinelBrakeLimit)

	// Once the window clears, the next re-emission convicts.
	c.now = func() time.Time { return base.Add(vgpuSentinelBrakeWindow + time.Second) }
	for _, target := range targets {
		if !c.tails[target.instanceID].done {
			appendSentinelLine(t, target.appLogPath)
		}
	}
	c.scanOnce(context.Background())
	assert.Len(t, *quarantined, vgpuSentinelBrakeLimit+1)
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
	c, quarantined := newTestSentinelController(t, store)
	c.isQuarantined = func(vf string) bool { return vf == "0000:e3:00.4" }

	// A rescan of a standing victim's log after a controller restart must
	// not re-convict a persisted quarantine: no brake accounting, no metric,
	// and the tail closes.
	c.scanOnce(context.Background())
	assert.Empty(t, *quarantined)
	assert.Empty(t, c.recent)
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

func appendSentinelLine(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	require.NoError(t, err)
	_, err = f.WriteString(testSentinelLine)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}
