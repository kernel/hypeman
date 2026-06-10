package guestmemory

import "testing"

func TestPlanGuestTargetsLargeCeilingSplitDoesNotOverflow(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)
	const assigned = 8 * gib
	const floor = gib / 2
	const headroom = assigned - floor // 7.5 GiB, exceeds ~2.8 GiB

	// totalReclaim * maxReclaimBytes overflows int64 once the operands exceed ~2.8
	// GiB. With the naive multiply the wrapped (negative) reclaim corrupts the
	// proportional split: one VM absorbs everything down to its floor while its
	// peer gives up nothing. The 128-bit intermediate keeps the split even.
	candidates := []candidateState{
		{vm: BalloonVM{ID: "a", AssignedMemoryBytes: assigned}, currentTargetGuestBytes: assigned, protectedFloorBytes: floor, maxReclaimBytes: headroom},
		{vm: BalloonVM{ID: "b", AssignedMemoryBytes: assigned}, currentTargetGuestBytes: assigned, protectedFloorBytes: floor, maxReclaimBytes: headroom},
	}

	targets := planGuestTargets(ActiveBallooningConfig{}, candidates, headroom)
	wantEach := assigned - headroom/2
	if targets["a"] != wantEach || targets["b"] != wantEach {
		t.Fatalf("identical VMs should split reclaim evenly to %d each, got a=%d b=%d", wantEach, targets["a"], targets["b"])
	}
}

func TestFloorAnchorBytesUsesBaselineForCeilingVM(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)
	const baseline = 1 * gib
	const ceiling = 4 * gib

	// A ceiling VM anchors its protected floor on the baseline it is held at, not
	// the ceiling, so it stays reclaimable down toward the baseline under pressure.
	if got := floorAnchorBytes(BalloonVM{AssignedMemoryBytes: ceiling, BaselineMemoryBytes: baseline}); got != baseline {
		t.Fatalf("ceiling VM should anchor on baseline %d, got %d", baseline, got)
	}
	// An ordinary VM (no smaller baseline) anchors on its assigned size, unchanged.
	if got := floorAnchorBytes(BalloonVM{AssignedMemoryBytes: ceiling, BaselineMemoryBytes: ceiling}); got != ceiling {
		t.Fatalf("ordinary VM should anchor on assigned %d, got %d", ceiling, got)
	}

	cfg := ActiveBallooningConfig{ProtectedFloorPercent: 50}
	if got := protectedFloorBytes(cfg, floorAnchorBytes(BalloonVM{AssignedMemoryBytes: ceiling, BaselineMemoryBytes: baseline})); got != baseline/2 {
		t.Fatalf("ceiling VM floor should be half its baseline %d, got %d", baseline/2, got)
	}
}

func TestProtectedFloorBytesCappedAtAnchor(t *testing.T) {
	const mib = int64(1024 * 1024)
	const baseline = 256 * mib

	cfg := ActiveBallooningConfig{
		ProtectedFloorPercent:  50,
		ProtectedFloorMinBytes: 512 * mib,
	}

	// The protected floor must never exceed the baseline anchor; otherwise a
	// small-baseline ceiling VM is forced above baseline on healthy reconciles.
	if got := protectedFloorBytes(cfg, baseline); got != baseline {
		t.Fatalf("protected floor should cap at anchor %d, got %d", baseline, got)
	}
}

func TestAutomaticTargetBytesStressedHoldsCurrentReclaim(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)
	cfg := ActiveBallooningConfig{PressureLowWatermarkAvailablePercent: 15}

	// Healthy: never reclaim.
	if got := automaticTargetBytes(HostPressureStateHealthy, cfg, HostPressureSample{TotalBytes: 64 * gib, AvailableBytes: 32 * gib}, 5*gib); got != 0 {
		t.Fatalf("healthy host should reclaim 0, got %d", got)
	}
	// Pressure, above the low watermark, stressed: hold the current reclaim level.
	if got := automaticTargetBytes(HostPressureStatePressure, cfg, HostPressureSample{TotalBytes: 64 * gib, AvailableBytes: 32 * gib, Stressed: true}, 3*gib); got != 3*gib {
		t.Fatalf("stressed above watermark should hold current reclaim 3GiB, got %d", got)
	}
	// Pressure, above the low watermark, not stressed: nothing to reclaim.
	if got := automaticTargetBytes(HostPressureStatePressure, cfg, HostPressureSample{TotalBytes: 64 * gib, AvailableBytes: 32 * gib}, 3*gib); got != 0 {
		t.Fatalf("unstressed above watermark should reclaim 0, got %d", got)
	}
}

func TestGrowthTargetBytesDisabled(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)
	cfg := DefaultActiveBallooningConfig() // GrowOnDemandEnabled defaults to false

	// Disabled: holds at the baseline regardless of utilization, leaving today's
	// reclaim-only behavior unchanged.
	for _, util := range []int{0, 50, 100} {
		got := growthTargetBytes(cfg, gib, 4*gib, gib/2, util)
		if got != gib {
			t.Fatalf("disabled grow: util=%d want hold at %d, got %d", util, gib, got)
		}
	}
}

func TestGrowthTargetBytesEnabled(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)
	cfg := DefaultActiveBallooningConfig()
	cfg.GrowOnDemandEnabled = true
	cfg.GrowUtilizationPercent = 85

	baseline := gib
	ceiling := 4 * gib
	floor := gib / 2

	// Below the threshold: hold at baseline.
	if got := growthTargetBytes(cfg, baseline, ceiling, floor, 84); got != baseline {
		t.Fatalf("below threshold should hold at baseline %d, got %d", baseline, got)
	}

	// At/above the threshold: grow to the ceiling, never beyond it.
	for _, util := range []int{85, 99, 100} {
		got := growthTargetBytes(cfg, baseline, ceiling, floor, util)
		if got != ceiling {
			t.Fatalf("util=%d should grow to ceiling %d, got %d", util, ceiling, got)
		}
	}

	// Never exceeds the ceiling even if the hold target somehow already sits at it.
	if got := growthTargetBytes(cfg, ceiling, ceiling, floor, 100); got != ceiling {
		t.Fatalf("grow must not exceed ceiling %d, got %d", ceiling, got)
	}
}

func TestGrowthTargetBytesRespectsFloor(t *testing.T) {
	const mib = int64(1024 * 1024)
	cfg := DefaultActiveBallooningConfig()
	cfg.GrowOnDemandEnabled = true
	cfg.GrowUtilizationPercent = 85

	// A baseline below the protected floor is clamped up to the floor, never
	// below it, on the hold path.
	floor := 512 * mib
	if got := growthTargetBytes(cfg, 128*mib, 1024*mib, floor, 10); got != floor {
		t.Fatalf("hold target below floor should clamp to floor %d, got %d", floor, got)
	}
}

func TestActiveBallooningNormalizeGrowFields(t *testing.T) {
	d := DefaultActiveBallooningConfig()

	// Unset/invalid GrowUtilizationPercent falls back to the default.
	for _, v := range []int{0, -5, 100, 250} {
		cfg := ActiveBallooningConfig{GrowUtilizationPercent: v}.Normalize()
		if cfg.GrowUtilizationPercent != d.GrowUtilizationPercent {
			t.Fatalf("GrowUtilizationPercent=%d should normalize to %d, got %d", v, d.GrowUtilizationPercent, cfg.GrowUtilizationPercent)
		}
	}

	// A valid value is preserved.
	cfg := ActiveBallooningConfig{GrowUtilizationPercent: 70}.Normalize()
	if cfg.GrowUtilizationPercent != 70 {
		t.Fatalf("valid GrowUtilizationPercent=70 should be preserved, got %d", cfg.GrowUtilizationPercent)
	}

	// GrowOnDemandEnabled is carried through untouched.
	if !(ActiveBallooningConfig{GrowOnDemandEnabled: true}).Normalize().GrowOnDemandEnabled {
		t.Fatal("GrowOnDemandEnabled=true should survive Normalize")
	}
	if (ActiveBallooningConfig{GrowOnDemandEnabled: false}).Normalize().GrowOnDemandEnabled {
		t.Fatal("GrowOnDemandEnabled=false should survive Normalize")
	}
}
