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
func (s *stubHypervisor) Pause(ctx context.Context) error  { return nil }
func (s *stubHypervisor) Resume(ctx context.Context) error { return nil }
func (s *stubHypervisor) Snapshot(ctx context.Context, destPath string, _ hypervisor.SnapshotOptions) error {
	return nil
}
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

func TestHealthyHoldsCeilingVMAtBaseline(t *testing.T) {
	const mib = int64(1024 * 1024)
	const baseline = 1024 * mib
	const ceiling = 4096 * mib

	// A ceiling VM: assigned is the ceiling, baseline is the smaller running size,
	// and the balloon currently sits at the baseline (post boot-to-baseline).
	src := &stubSource{
		vms: []BalloonVM{
			{ID: "a", Name: "a", HypervisorType: hypervisor.TypeVZ, SocketPath: "a", AssignedMemoryBytes: ceiling, BaselineMemoryBytes: baseline},
		},
	}
	hv := &stubHypervisor{target: baseline, capabilities: hypervisor.Capabilities{SupportsBalloonControl: true}}

	c := NewController(Policy{Enabled: true, ReclaimEnabled: true}, ActiveBallooningConfig{
		Enabled:                true,
		ProtectedFloorPercent:  50,
		ProtectedFloorMinBytes: 0,
		MinAdjustmentBytes:     1,
		PerVMMaxStepBytes:      ceiling,
		PerVMCooldown:          time.Millisecond,
		// Grow-on-demand off: the controller must not grow the guest toward the
		// ceiling on a healthy host.
		GrowOnDemandEnabled: false,
	}, src, slog.New(slog.NewTextHandler(io.Discard, nil))).(*controller)
	c.sampler = &stubSampler{sample: HostPressureSample{TotalBytes: 64 * 1024 * mib, AvailableBytes: 32 * 1024 * mib, AvailablePercent: 50}}
	c.reconcileMu.newClient = func(_ hypervisor.Type, _ string) (hypervisor.Hypervisor, error) {
		return hv, nil
	}

	resp, err := c.TriggerReclaim(context.Background(), ManualReclaimRequest{ReclaimBytes: 0})
	require.NoError(t, err)
	require.Len(t, resp.Actions, 1)
	assert.Equal(t, "unchanged", resp.Actions[0].Status)
	assert.Equal(t, baseline, resp.Actions[0].TargetGuestMemoryBytes, "ceiling VM should hold at baseline, not grow to ceiling, when grow-on-demand is off")
	assert.Equal(t, baseline, hv.target, "balloon target must remain at baseline")
	assert.Equal(t, int64(0), resp.PlannedReclaimBytes, "a ceiling VM held at baseline reclaims nothing; its idle headroom must not be counted as reclaim")
}

func TestHealthyPreservesGrownCeilingVM(t *testing.T) {
	const mib = int64(1024 * 1024)
	const baseline = 1024 * mib
	const ceiling = 4096 * mib
	const grown = 3072 * mib

	// A ceiling VM deliberately grown above its baseline (e.g. via the balloon API)
	// must not be reverted to baseline by the controller on a healthy host: the
	// controller recovers reclaimed guests up to baseline, but does not undo grows.
	src := &stubSource{
		vms: []BalloonVM{
			{ID: "a", Name: "a", HypervisorType: hypervisor.TypeVZ, SocketPath: "a", AssignedMemoryBytes: ceiling, BaselineMemoryBytes: baseline},
		},
	}
	hv := &stubHypervisor{target: grown, capabilities: hypervisor.Capabilities{SupportsBalloonControl: true}}

	c := NewController(Policy{Enabled: true, ReclaimEnabled: true}, ActiveBallooningConfig{
		Enabled:                true,
		ProtectedFloorPercent:  50,
		ProtectedFloorMinBytes: 0,
		MinAdjustmentBytes:     1,
		PerVMMaxStepBytes:      ceiling,
		PerVMCooldown:          time.Millisecond,
		GrowOnDemandEnabled:    false,
	}, src, slog.New(slog.NewTextHandler(io.Discard, nil))).(*controller)
	c.sampler = &stubSampler{sample: HostPressureSample{TotalBytes: 64 * 1024 * mib, AvailableBytes: 32 * 1024 * mib, AvailablePercent: 50}}
	c.reconcileMu.newClient = func(_ hypervisor.Type, _ string) (hypervisor.Hypervisor, error) {
		return hv, nil
	}

	_, err := c.TriggerReclaim(context.Background(), ManualReclaimRequest{ReclaimBytes: 0})
	require.NoError(t, err)
	assert.Equal(t, grown, hv.target, "a deliberately grown ceiling VM must not be reverted to baseline while healthy")
}

func TestStressedCeilingVMAtBaselineDoesNotSqueezeCoTenant(t *testing.T) {
	const mib = int64(1024 * 1024)
	const baseline = 1024 * mib
	const ceiling = 4096 * mib
	const coTenant = 2048 * mib

	// A ceiling VM idling at its baseline is reclaiming nothing real (its ballooned
	// headroom was never resident), so under the Stressed branch — where the host
	// reports stress but is still above the low watermark — its headroom must not be
	// counted as reclaim and redistributed onto a co-tenant.
	src := &stubSource{
		vms: []BalloonVM{
			{ID: "ceiling", Name: "ceiling", HypervisorType: hypervisor.TypeVZ, SocketPath: "ceiling", AssignedMemoryBytes: ceiling, BaselineMemoryBytes: baseline},
			{ID: "ordinary", Name: "ordinary", HypervisorType: hypervisor.TypeCloudHypervisor, SocketPath: "ordinary", AssignedMemoryBytes: coTenant, BaselineMemoryBytes: coTenant},
		},
	}
	ceilingHV := &stubHypervisor{target: baseline, capabilities: hypervisor.Capabilities{SupportsBalloonControl: true}}
	ordinaryHV := &stubHypervisor{target: coTenant, capabilities: hypervisor.Capabilities{SupportsBalloonControl: true}}

	c := NewController(Policy{Enabled: true, ReclaimEnabled: true}, ActiveBallooningConfig{
		Enabled:                               true,
		PressureHighWatermarkAvailablePercent: 10,
		PressureLowWatermarkAvailablePercent:  15,
		ProtectedFloorPercent:                 50,
		ProtectedFloorMinBytes:                0,
		MinAdjustmentBytes:                    1,
		PerVMMaxStepBytes:                     ceiling,
		PerVMCooldown:                         time.Millisecond,
	}, src, slog.New(slog.NewTextHandler(io.Discard, nil))).(*controller)
	// Stressed, but available (32/64 GiB) is well above the low watermark, so the
	// watermark math demands no reclaim and automaticTargetBytes falls to the
	// Stressed currentTotalReclaim branch.
	c.sampler = &stubSampler{sample: HostPressureSample{TotalBytes: 64 * 1024 * mib, AvailableBytes: 32 * 1024 * mib, AvailablePercent: 50, Stressed: true}}
	c.reconcileMu.newClient = func(_ hypervisor.Type, socket string) (hypervisor.Hypervisor, error) {
		if socket == "ceiling" {
			return ceilingHV, nil
		}
		return ordinaryHV, nil
	}

	_, err := c.TriggerReclaim(context.Background(), ManualReclaimRequest{ReclaimBytes: 0})
	require.NoError(t, err)
	assert.Equal(t, coTenant, ordinaryHV.target, "ordinary co-tenant must not be reclaimed for the ceiling VM's idle headroom")
	assert.Equal(t, baseline, ceilingHV.target, "ceiling VM holds at baseline")
}

func TestPressureReclaimsCeilingVMTowardFloorNotCeiling(t *testing.T) {
	const mib = int64(1024 * 1024)
	const baseline = 1024 * mib
	const ceiling = 4096 * mib

	// Under genuine host pressure (available below the low watermark, so the
	// watermark math demands real reclaim), a ceiling VM idling at its baseline
	// must be reclaimed toward its floor — never inflated toward the ceiling. The
	// reclaim target is anchored on the baseline, not the boot ceiling.
	src := &stubSource{
		vms: []BalloonVM{
			{ID: "ceiling", Name: "ceiling", HypervisorType: hypervisor.TypeVZ, SocketPath: "ceiling", AssignedMemoryBytes: ceiling, BaselineMemoryBytes: baseline},
		},
	}
	ceilingHV := &stubHypervisor{target: baseline, capabilities: hypervisor.Capabilities{SupportsBalloonControl: true}}

	c := NewController(Policy{Enabled: true, ReclaimEnabled: true}, ActiveBallooningConfig{
		Enabled:                               true,
		PressureHighWatermarkAvailablePercent: 10,
		PressureLowWatermarkAvailablePercent:  15,
		ProtectedFloorPercent:                 50,
		ProtectedFloorMinBytes:                0,
		MinAdjustmentBytes:                    1,
		PerVMMaxStepBytes:                     ceiling,
		PerVMCooldown:                         time.Millisecond,
	}, src, slog.New(slog.NewTextHandler(io.Discard, nil))).(*controller)
	// Available (256 MiB of 8 GiB ~= 3%) is below the low watermark, so reclaim is
	// genuinely demanded and automaticTargetBytes returns a positive target.
	c.sampler = &stubSampler{sample: HostPressureSample{TotalBytes: 8192 * mib, AvailableBytes: 256 * mib, AvailablePercent: 3}}
	c.reconcileMu.newClient = func(_ hypervisor.Type, _ string) (hypervisor.Hypervisor, error) {
		return ceilingHV, nil
	}

	_, err := c.TriggerReclaim(context.Background(), ManualReclaimRequest{ReclaimBytes: 0})
	require.NoError(t, err)
	assert.LessOrEqual(t, ceilingHV.target, baseline, "ceiling VM must be reclaimed under pressure, never inflated above its baseline")
	assert.GreaterOrEqual(t, ceilingHV.target, baseline/2, "ceiling VM must not be reclaimed below its protected floor")
}

func TestHealthyRecoversReclaimedVMToBaseline(t *testing.T) {
	const mib = int64(1024 * 1024)
	const assigned = 2048 * mib

	// An ordinary VM (baseline == assigned) whose balloon was previously inflated
	// (target below assigned, e.g. after pressure) recovers toward assigned when
	// the host is healthy — the existing reclaim-recovery behavior, unchanged.
	src := &stubSource{
		vms: []BalloonVM{
			{ID: "a", Name: "a", HypervisorType: hypervisor.TypeCloudHypervisor, SocketPath: "a", AssignedMemoryBytes: assigned, BaselineMemoryBytes: assigned},
		},
	}
	hv := &stubHypervisor{target: 1024 * mib, capabilities: hypervisor.Capabilities{SupportsBalloonControl: true}}

	c := NewController(Policy{Enabled: true, ReclaimEnabled: true}, ActiveBallooningConfig{
		Enabled:                true,
		ProtectedFloorPercent:  50,
		ProtectedFloorMinBytes: 0,
		MinAdjustmentBytes:     1,
		PerVMMaxStepBytes:      assigned,
		PerVMCooldown:          time.Millisecond,
	}, src, slog.New(slog.NewTextHandler(io.Discard, nil))).(*controller)
	c.sampler = &stubSampler{sample: HostPressureSample{TotalBytes: 64 * 1024 * mib, AvailableBytes: 32 * 1024 * mib, AvailablePercent: 50}}
	c.reconcileMu.newClient = func(_ hypervisor.Type, _ string) (hypervisor.Hypervisor, error) {
		return hv, nil
	}

	resp, err := c.TriggerReclaim(context.Background(), ManualReclaimRequest{ReclaimBytes: 0})
	require.NoError(t, err)
	require.Len(t, resp.Actions, 1)
	assert.Equal(t, "applied", resp.Actions[0].Status)
	assert.Equal(t, assigned, hv.target, "healthy host should recover the balloon back to assigned")
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

func TestTriggerReclaimDryRunDoesNotReportAppliedReclaim(t *testing.T) {
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

	resp, err := c.TriggerReclaim(context.Background(), ManualReclaimRequest{
		ReclaimBytes: 256 * mib,
		DryRun:       true,
		HoldFor:      30 * time.Second,
	})
	require.NoError(t, err)
	require.Len(t, resp.Actions, 1)
	assert.Equal(t, int64(256*mib), resp.PlannedReclaimBytes)
	assert.Equal(t, int64(0), resp.AppliedReclaimBytes, "dry-run should not report applied reclaim")
	assert.Equal(t, "planned", resp.Actions[0].Status)
	assert.Equal(t, int64(0), resp.Actions[0].AppliedReclaimBytes, "dry-run actions should not report applied reclaim")
	assert.Equal(t, int64(1024*mib), hv.target, "dry-run should not mutate the hypervisor target")
}
