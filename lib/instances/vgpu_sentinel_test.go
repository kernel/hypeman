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

const testSentinelLine = "[   27.031415] NVRM: GPU 0000:e3:00.4: RmInitAdapter failed! (0x22:0x65:884)\n"

func TestVGPUSentinelPattern(t *testing.T) {
	t.Parallel()

	assert.True(t, vgpuSentinelPattern.MatchString(testSentinelLine))
	// Tuple values are driver-build-specific; the match must not depend on them.
	assert.True(t, vgpuSentinelPattern.MatchString("[  47.2] NVRM: GPU 0000:e3:00.4: RmInitAdapter failed! (0x26:0xffff:1482)"))

	// Echoed exec command lines carry the bare token on the same console.
	assert.False(t, vgpuSentinelPattern.MatchString("$ dmesg | grep -c RmInitAdapter"))
	assert.False(t, vgpuSentinelPattern.MatchString("[    7.1] NVRM: loading NVIDIA UNIX Open Kernel Module for x86_64"))
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
		quarantine: func(q devices.VFQuarantine) (devices.VFHealthRecord, error) {
			quarantined = append(quarantined, q)
			return devices.VFHealthRecord{VFAddress: q.VFAddress, WedgeCount: 1}, nil
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
	_, err = f.WriteString("[   27.03] NVRM: GPU 0000:e3:00.4: RmInitAdapter failed")
	require.NoError(t, err)
	c.scanOnce(ctx)
	assert.Empty(t, *quarantined)

	_, err = f.WriteString("! (0x22:0x65:884)\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	c.scanOnce(ctx)
	require.Len(t, *quarantined, 1)
	assert.Equal(t, "0000:e3:00.4", (*quarantined)[0].VFAddress)
	assert.Equal(t, "instance-1", (*quarantined)[0].InstanceID)
	assert.Contains(t, (*quarantined)[0].SentinelLine, "RmInitAdapter failed!")

	// The sentinel recurs every ~20s; a convicted instance is not re-convicted.
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
	c.quarantine = func(devices.VFQuarantine) (devices.VFHealthRecord, error) {
		return devices.VFHealthRecord{}, quarantineErr
	}
	ctx := context.Background()

	c.scanOnce(ctx)
	assert.Empty(t, *quarantined)
	assert.False(t, c.tails["instance-1"].done)

	// The next recurrence of the sentinel retries the quarantine.
	c.quarantine = realQuarantine
	appendSentinelLine(t, logPath)
	c.scanOnce(ctx)
	assert.Len(t, *quarantined, 1)
	assert.True(t, c.tails["instance-1"].done)
}

func TestVGPUSentinelControllerBrakeSuppressesConvictionBursts(t *testing.T) {
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

	c.scanOnce(context.Background())
	// The burst converts to convictions up to the limit; the rest are
	// suppressed rather than quarantining the whole host.
	assert.Len(t, *quarantined, vgpuSentinelBrakeLimit)
	for _, target := range targets {
		assert.True(t, c.tails[target.instanceID].done)
	}
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
