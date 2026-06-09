package guestmemory

import "testing"

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
