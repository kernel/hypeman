package guestmemory

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubSource struct {
	vms []BalloonVM
	err error
}

func (s *stubSource) ListBalloonVMs(ctx context.Context) ([]BalloonVM, error) {
	_ = ctx
	if s.err != nil {
		return nil, s.err
	}
	return s.vms, nil
}

type stubSampler struct {
	sample HostPressureSample
	err    error
}

func (s *stubSampler) Sample(ctx context.Context) (HostPressureSample, error) {
	_ = ctx
	return s.sample, s.err
}

type stubHypervisor struct {
	target       int64
	capabilities hypervisor.Capabilities
	setErr       error
}

func (s *stubHypervisor) DeleteVM(ctx context.Context) error { return nil }
func (s *stubHypervisor) Shutdown(ctx context.Context) error { return nil }
func (s *stubHypervisor) GetVMInfo(ctx context.Context) (*hypervisor.VMInfo, error) {
	return &hypervisor.VMInfo{State: hypervisor.StateRunning}, nil
}
func (s *stubHypervisor) Pause(ctx context.Context) error                     { return nil }
func (s *stubHypervisor) Resume(ctx context.Context) error                    { return nil }
func (s *stubHypervisor) Snapshot(ctx context.Context, destPath string) error { return nil }
func (s *stubHypervisor) ResizeMemory(ctx context.Context, bytes int64) error { return nil }
func (s *stubHypervisor) ResizeMemoryAndWait(ctx context.Context, bytes int64, timeout time.Duration) error {
	return nil
}
func (s *stubHypervisor) Capabilities() hypervisor.Capabilities { return s.capabilities }
func (s *stubHypervisor) SetTargetGuestMemoryBytes(ctx context.Context, bytes int64) error {
	_ = ctx
	if s.setErr != nil {
		return s.setErr
	}
	s.target = bytes
	return nil
}
func (s *stubHypervisor) GetTargetGuestMemoryBytes(ctx context.Context) (int64, error) {
	_ = ctx
	return s.target, nil
}

func TestTriggerReclaimDistributesProportionally(t *testing.T) {
	const mib = int64(1024 * 1024)
	src := &stubSource{
		vms: []BalloonVM{
			{ID: "a", Name: "a", HypervisorType: hypervisor.TypeCloudHypervisor, SocketPath: "a", AssignedMemoryBytes: 1024 * mib},
			{ID: "b", Name: "b", HypervisorType: hypervisor.TypeCloudHypervisor, SocketPath: "b", AssignedMemoryBytes: 2048 * mib},
		},
	}
	hvA := &stubHypervisor{target: 1024 * mib, capabilities: hypervisor.Capabilities{SupportsBalloonControl: true}}
	hvB := &stubHypervisor{target: 2048 * mib, capabilities: hypervisor.Capabilities{SupportsBalloonControl: true}}

	c := NewController(Policy{Enabled: true, ReclaimEnabled: true}, ActiveBallooningConfig{
		Enabled:                true,
		ProtectedFloorPercent:  50,
		ProtectedFloorMinBytes: 0,
		MinAdjustmentBytes:     1,
		PerVMMaxStepBytes:      4096 * mib,
		PerVMCooldown:          time.Second,
	}, src, slog.New(slog.NewTextHandler(io.Discard, nil))).(*controller)
	c.sampler = &stubSampler{sample: HostPressureSample{TotalBytes: 4096 * mib, AvailableBytes: 4096 * mib, AvailablePercent: 100}}
	c.reconcileMu.newClient = func(t hypervisor.Type, socket string) (hypervisor.Hypervisor, error) {
		switch socket {
		case "a":
			return hvA, nil
		case "b":
			return hvB, nil
		default:
			return nil, errors.New("unknown")
		}
	}

	resp, err := c.TriggerReclaim(context.Background(), ManualReclaimRequest{ReclaimBytes: 768 * mib, HoldFor: time.Minute})
	require.NoError(t, err)
	assert.Equal(t, int64(768*mib), resp.PlannedReclaimBytes)
	assert.Equal(t, int64(768*mib), resp.AppliedReclaimBytes)
	assert.Equal(t, int64(768*mib), 1024*mib-hvA.target+2048*mib-hvB.target)
	assert.Equal(t, int64(768*mib), resp.Actions[0].AppliedReclaimBytes+resp.Actions[1].AppliedReclaimBytes)
}

func TestPressureStateUsesHysteresis(t *testing.T) {
	cfg := DefaultActiveBallooningConfig()
	cfg.PressureHighWatermarkAvailablePercent = 10
	cfg.PressureLowWatermarkAvailablePercent = 15

	assert.Equal(t, HostPressureStatePressure, nextPressureState(HostPressureStateHealthy, cfg, HostPressureSample{AvailablePercent: 9}))
	assert.Equal(t, HostPressureStateHealthy, nextPressureState(HostPressureStateHealthy, cfg, HostPressureSample{AvailablePercent: 10}))
	assert.Equal(t, HostPressureStateHealthy, nextPressureState(HostPressureStateHealthy, cfg, HostPressureSample{AvailablePercent: 10.9}))
	assert.Equal(t, HostPressureStatePressure, nextPressureState(HostPressureStatePressure, cfg, HostPressureSample{AvailablePercent: 12}))
	assert.Equal(t, HostPressureStatePressure, nextPressureState(HostPressureStatePressure, cfg, HostPressureSample{AvailablePercent: 14.9}))
	assert.Equal(t, HostPressureStateHealthy, nextPressureState(HostPressureStatePressure, cfg, HostPressureSample{AvailablePercent: 16}))
}

func TestTriggerReclaimReturnsWhenContextIsCanceledWhileWaitingForLock(t *testing.T) {
	const mib = int64(1024 * 1024)

	src := &stubSource{
		vms: []BalloonVM{
			{ID: "a", Name: "a", HypervisorType: hypervisor.TypeCloudHypervisor, SocketPath: "a", AssignedMemoryBytes: 1024 * mib},
		},
	}

	c := NewController(Policy{Enabled: true, ReclaimEnabled: true}, ActiveBallooningConfig{
		Enabled:                true,
		ProtectedFloorPercent:  50,
		ProtectedFloorMinBytes: 0,
		MinAdjustmentBytes:     1,
		PerVMMaxStepBytes:      4096 * mib,
		PerVMCooldown:          time.Second,
	}, src, slog.New(slog.NewTextHandler(io.Discard, nil))).(*controller)
	c.sampler = &stubSampler{sample: HostPressureSample{TotalBytes: 1024 * mib, AvailableBytes: 1024 * mib, AvailablePercent: 100}}

	<-c.reconcileMu.mu
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.TriggerReclaim(ctx, ManualReclaimRequest{ReclaimBytes: 128 * mib})
	require.ErrorIs(t, err, context.Canceled)

	c.reconcileMu.mu <- struct{}{}
}

func TestTriggerReclaimMinAdjustmentKeepsCurrentTarget(t *testing.T) {
	const mib = int64(1024 * 1024)

	src := &stubSource{
		vms: []BalloonVM{
			{ID: "a", Name: "a", HypervisorType: hypervisor.TypeCloudHypervisor, SocketPath: "a", AssignedMemoryBytes: 1024 * mib},
		},
	}
	hv := &stubHypervisor{target: 1024 * mib, capabilities: hypervisor.Capabilities{SupportsBalloonControl: true}}

	c := NewController(Policy{Enabled: true, ReclaimEnabled: true}, ActiveBallooningConfig{
		Enabled:                true,
		ProtectedFloorPercent:  50,
		ProtectedFloorMinBytes: 0,
		MinAdjustmentBytes:     64 * mib,
		PerVMMaxStepBytes:      64 * mib,
		PerVMCooldown:          time.Minute,
	}, src, slog.New(slog.NewTextHandler(io.Discard, nil))).(*controller)
	c.sampler = &stubSampler{sample: HostPressureSample{TotalBytes: 1024 * mib, AvailableBytes: 1024 * mib, AvailablePercent: 100}}
	c.reconcileMu.newClient = func(t hypervisor.Type, socket string) (hypervisor.Hypervisor, error) {
		return hv, nil
	}

	resp, err := c.TriggerReclaim(context.Background(), ManualReclaimRequest{ReclaimBytes: 32 * mib})
	require.NoError(t, err)
	require.Len(t, resp.Actions, 1)
	assert.Equal(t, "unchanged", resp.Actions[0].Status)
	assert.Equal(t, int64(1024*mib), resp.Actions[0].TargetGuestMemoryBytes)
}

func TestTriggerReclaimRespectsProtectedFloor(t *testing.T) {
	const mib = int64(1024 * 1024)
	src := &stubSource{
		vms: []BalloonVM{
			{ID: "a", Name: "a", HypervisorType: hypervisor.TypeCloudHypervisor, SocketPath: "a", AssignedMemoryBytes: 1024 * mib},
		},
	}
	hv := &stubHypervisor{target: 1024 * mib, capabilities: hypervisor.Capabilities{SupportsBalloonControl: true}}
	c := NewController(Policy{Enabled: true, ReclaimEnabled: true}, ActiveBallooningConfig{
		Enabled:                true,
		ProtectedFloorPercent:  75,
		ProtectedFloorMinBytes: 0,
		MinAdjustmentBytes:     1,
		PerVMMaxStepBytes:      4096 * mib,
		PerVMCooldown:          time.Second,
	}, src, slog.New(slog.NewTextHandler(io.Discard, nil))).(*controller)
	c.sampler = &stubSampler{sample: HostPressureSample{TotalBytes: 1024 * mib, AvailableBytes: 1024 * mib, AvailablePercent: 100}}
	c.reconcileMu.newClient = func(t hypervisor.Type, socket string) (hypervisor.Hypervisor, error) {
		return hv, nil
	}

	resp, err := c.TriggerReclaim(context.Background(), ManualReclaimRequest{ReclaimBytes: 1024 * mib, HoldFor: time.Minute})
	require.NoError(t, err)
	require.Len(t, resp.Actions, 1)
	assert.Equal(t, int64(768*mib), resp.Actions[0].TargetGuestMemoryBytes)
	assert.Equal(t, int64(256*mib), resp.AppliedReclaimBytes)
}

func TestTriggerReclaimWithoutHoldAppliesRequestedReclaim(t *testing.T) {
	const mib = int64(1024 * 1024)
	src := &stubSource{
		vms: []BalloonVM{
			{ID: "a", Name: "a", HypervisorType: hypervisor.TypeCloudHypervisor, SocketPath: "a", AssignedMemoryBytes: 1024 * mib},
		},
	}
	hv := &stubHypervisor{target: 1024 * mib, capabilities: hypervisor.Capabilities{SupportsBalloonControl: true}}
	c := NewController(Policy{Enabled: true, ReclaimEnabled: true}, ActiveBallooningConfig{
		Enabled:                true,
		ProtectedFloorPercent:  50,
		ProtectedFloorMinBytes: 0,
		MinAdjustmentBytes:     1,
		PerVMMaxStepBytes:      4096 * mib,
		PerVMCooldown:          time.Second,
	}, src, slog.New(slog.NewTextHandler(io.Discard, nil))).(*controller)
	c.sampler = &stubSampler{sample: HostPressureSample{TotalBytes: 1024 * mib, AvailableBytes: 1024 * mib, AvailablePercent: 100}}
	c.reconcileMu.newClient = func(t hypervisor.Type, socket string) (hypervisor.Hypervisor, error) {
		return hv, nil
	}

	resp, err := c.TriggerReclaim(context.Background(), ManualReclaimRequest{ReclaimBytes: 256 * mib, HoldFor: 0})
	require.NoError(t, err)
	require.Len(t, resp.Actions, 1)
	assert.Equal(t, int64(768*mib), resp.Actions[0].TargetGuestMemoryBytes)
	assert.Equal(t, int64(256*mib), resp.AppliedReclaimBytes)
	assert.Nil(t, resp.HoldUntil)

	followup, err := c.TriggerReclaim(context.Background(), ManualReclaimRequest{})
	require.NoError(t, err)
	assert.Equal(t, int64(0), followup.AppliedReclaimBytes)
	assert.Equal(t, int64(1024*mib), hv.target)
}
