package instances

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/guestmemory"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type staticBalloonSource struct {
	vms []guestmemory.BalloonVM
}

type fixedPressureSampler struct {
	sample guestmemory.HostPressureSample
}

func (s *staticBalloonSource) ListBalloonVMs(ctx context.Context) ([]guestmemory.BalloonVM, error) {
	_ = ctx
	return s.vms, nil
}

func (s *fixedPressureSampler) Sample(ctx context.Context) (guestmemory.HostPressureSample, error) {
	_ = ctx
	return s.sample, nil
}

func newActiveBallooningTestController(t *testing.T, inst *Instance) guestmemory.Controller {
	t.Helper()

	cfg := guestmemory.DefaultActiveBallooningConfig()
	cfg.Enabled = true
	cfg.MinAdjustmentBytes = 1
	cfg.PerVMMaxStepBytes = inst.Size + inst.HotplugSize
	cfg.PerVMCooldown = 1 * time.Millisecond

	return guestmemory.NewControllerWithSampler(
		guestmemory.Policy{
			Enabled:        true,
			ReclaimEnabled: true,
		},
		cfg,
		&staticBalloonSource{
			vms: []guestmemory.BalloonVM{
				{
					ID:                  inst.Id,
					Name:                inst.Name,
					HypervisorType:      inst.HypervisorType,
					SocketPath:          inst.SocketPath,
					AssignedMemoryBytes: inst.Size + inst.HotplugSize,
				},
			},
		},
		&fixedPressureSampler{
			sample: guestmemory.HostPressureSample{
				TotalBytes:       64 * 1024 * 1024 * 1024,
				AvailableBytes:   32 * 1024 * 1024 * 1024,
				AvailablePercent: 50,
				Stressed:         false,
			},
		},
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	)
}

func requireRuntimeGuestMemoryTarget(t *testing.T, ctx context.Context, inst *Instance) int64 {
	t.Helper()

	hv, err := hypervisor.NewClient(inst.HypervisorType, inst.SocketPath)
	require.NoError(t, err)

	target, err := hv.GetTargetGuestMemoryBytes(ctx)
	require.NoError(t, err)
	return target
}

func requireRuntimeGuestMemoryTargetEventually(t *testing.T, ctx context.Context, inst *Instance, expected int64) {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	var last int64
	var lastErr error

	for time.Now().Before(deadline) {
		hv, err := hypervisor.NewClient(inst.HypervisorType, inst.SocketPath)
		if err == nil {
			last, err = hv.GetTargetGuestMemoryBytes(ctx)
			lastErr = err
			if err == nil && last == expected {
				return
			}
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}

	require.NoError(t, lastErr)
	require.Equal(t, expected, last)
}

func requireManualReclaimApplied(t *testing.T, ctx context.Context, controller guestmemory.Controller, inst *Instance, reclaimBytes int64, holdFor time.Duration) guestmemory.ManualReclaimResponse {
	t.Helper()

	resp, err := controller.TriggerReclaim(ctx, guestmemory.ManualReclaimRequest{
		ReclaimBytes: reclaimBytes,
		HoldFor:      holdFor,
		Reason:       "integration-test",
	})
	require.NoError(t, err)
	requireRuntimeGuestMemoryTargetEventually(t, ctx, inst, inst.Size+inst.HotplugSize-resp.AppliedReclaimBytes)
	return resp
}

func requireManualReclaimCleared(t *testing.T, ctx context.Context, controller guestmemory.Controller, inst *Instance) guestmemory.ManualReclaimResponse {
	t.Helper()

	resp, err := controller.TriggerReclaim(ctx, guestmemory.ManualReclaimRequest{
		ReclaimBytes: 0,
		HoldFor:      0,
		Reason:       "integration-test-clear",
	})
	require.NoError(t, err)
	requireRuntimeGuestMemoryTargetEventually(t, ctx, inst, inst.Size+inst.HotplugSize)
	return resp
}

func assertActiveBallooningLifecycle(t *testing.T, ctx context.Context, inst *Instance) {
	t.Helper()

	assigned := inst.Size + inst.HotplugSize
	initialTarget := requireRuntimeGuestMemoryTarget(t, ctx, inst)
	assert.Equal(t, assigned, initialTarget, "runtime balloon target should start at full assigned memory")

	controller := newActiveBallooningTestController(t, inst)

	reclaimResp := requireManualReclaimApplied(t, ctx, controller, inst, 1*1024*1024*1024, 5*time.Minute)
	require.Len(t, reclaimResp.Actions, 1)
	assert.NotNil(t, reclaimResp.HoldUntil)
	assert.Equal(t, int64(1*1024*1024*1024), reclaimResp.Actions[0].AppliedReclaimBytes)
	assert.Equal(t, assigned-int64(1*1024*1024*1024), reclaimResp.Actions[0].TargetGuestMemoryBytes)

	clearResp := requireManualReclaimCleared(t, ctx, controller, inst)
	assert.Nil(t, clearResp.HoldUntil)

	floorResp := requireManualReclaimApplied(t, ctx, controller, inst, assigned, 5*time.Minute)
	require.Len(t, floorResp.Actions, 1)
	expectedFloor := assigned / 2
	assert.Equal(t, expectedFloor, floorResp.Actions[0].TargetGuestMemoryBytes)
	assert.Equal(t, assigned-expectedFloor, floorResp.Actions[0].AppliedReclaimBytes)

	requireManualReclaimCleared(t, ctx, controller, inst)
}
