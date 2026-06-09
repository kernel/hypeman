package guestmemory

import (
	"math/bits"

	"github.com/kernel/hypeman/lib/hypervisor"
)

type candidateState struct {
	vm                      BalloonVM
	hv                      hypervisor.Hypervisor
	currentTargetGuestBytes int64
	protectedFloorBytes     int64
	maxReclaimBytes         int64
}

// baselineGuestBytes is the target the controller holds when the host is
// healthy: the guest's baseline, clamped into [protectedFloor, assigned]. For
// ordinary VMs the baseline equals the assigned size.
func (c candidateState) baselineGuestBytes() int64 {
	baseline := c.vm.BaselineMemoryBytes
	if baseline <= 0 || baseline > c.vm.AssignedMemoryBytes {
		baseline = c.vm.AssignedMemoryBytes
	}
	return clampInt64(baseline, c.protectedFloorBytes, c.vm.AssignedMemoryBytes)
}

// utilizationPercent reports the guest's memory usage as a percentage of its
// current allowance. No measured guest-memory signal exists yet (RFC milestone
// 4), so this is 0 and auto-grow stays inert until one is wired in.
func (c candidateState) utilizationPercent() int {
	return 0
}

func planGuestTargets(cfg ActiveBallooningConfig, candidates []candidateState, totalReclaim int64) map[string]int64 {
	targets := make(map[string]int64, len(candidates))
	if len(candidates) == 0 {
		return targets
	}

	var totalHeadroom int64
	for _, candidate := range candidates {
		totalHeadroom += candidate.maxReclaimBytes
		targets[candidate.vm.ID] = floorAnchorBytes(candidate.vm)
	}
	if totalHeadroom <= 0 {
		return targets
	}

	totalReclaim = clampInt64(totalReclaim, 0, totalHeadroom)
	if totalReclaim == 0 {
		return targets
	}

	remainder := totalReclaim
	for _, candidate := range candidates {
		// totalReclaim and maxReclaimBytes can both approach totalHeadroom for a
		// large-ceiling VM, so the product overflows int64 once they exceed ~2.8
		// GiB; compute it in 128 bits to keep the proportional split correct.
		reclaim := mulDivInt64(totalReclaim, candidate.maxReclaimBytes, totalHeadroom)
		if reclaim > candidate.maxReclaimBytes {
			reclaim = candidate.maxReclaimBytes
		}
		// Reclaim down from the baseline anchor, not the ceiling: a ceiling VM
		// idles at its baseline, so subtracting from AssignedMemoryBytes (the
		// ceiling) would land above the baseline and inflate the guest under
		// pressure instead of reclaiming it. For ordinary VMs the anchor is the
		// assigned size, so this is unchanged.
		targets[candidate.vm.ID] = floorAnchorBytes(candidate.vm) - reclaim
		remainder -= reclaim
	}

	for _, candidate := range candidates {
		if remainder <= 0 {
			break
		}
		currentReclaim := floorAnchorBytes(candidate.vm) - targets[candidate.vm.ID]
		headroomLeft := candidate.maxReclaimBytes - currentReclaim
		if headroomLeft <= 0 {
			continue
		}
		extra := minInt64(headroomLeft, remainder)
		targets[candidate.vm.ID] -= extra
		remainder -= extra
	}

	return targets
}

func protectedFloorBytes(cfg ActiveBallooningConfig, anchor int64) int64 {
	percentFloor := (anchor * int64(cfg.ProtectedFloorPercent)) / 100
	return maxInt64(cfg.ProtectedFloorMinBytes, percentFloor)
}

// floorAnchorBytes is the size the protected floor is computed against: the
// guest's baseline (normal running size), or the assigned size when no smaller
// baseline is set (ordinary, non-ceiling VMs).
func floorAnchorBytes(vm BalloonVM) int64 {
	if vm.BaselineMemoryBytes > 0 && vm.BaselineMemoryBytes < vm.AssignedMemoryBytes {
		return vm.BaselineMemoryBytes
	}
	return vm.AssignedMemoryBytes
}

func nextPressureState(current HostPressureState, cfg ActiveBallooningConfig, sample HostPressureSample) HostPressureState {
	availablePercent := sample.AvailablePercent
	highWatermark := float64(cfg.PressureHighWatermarkAvailablePercent)
	lowWatermark := float64(cfg.PressureLowWatermarkAvailablePercent)

	switch current {
	case HostPressureStatePressure:
		if availablePercent >= lowWatermark && !sample.Stressed {
			return HostPressureStateHealthy
		}
		return HostPressureStatePressure
	default:
		if availablePercent < highWatermark || sample.Stressed {
			return HostPressureStatePressure
		}
		return HostPressureStateHealthy
	}
}

func automaticTargetBytes(state HostPressureState, cfg ActiveBallooningConfig, sample HostPressureSample, currentTotalReclaim int64) int64 {
	if state != HostPressureStatePressure || sample.TotalBytes <= 0 {
		return 0
	}
	lowWatermarkBytes := (sample.TotalBytes * int64(cfg.PressureLowWatermarkAvailablePercent)) / 100
	needed := lowWatermarkBytes - sample.AvailableBytes
	if needed > 0 {
		return needed
	}
	if sample.Stressed {
		return currentTotalReclaim
	}
	return 0
}

// growthTargetBytes returns the healthy-host target for a guest, bounded to
// [protectedFloor, assigned]. With GrowOnDemandEnabled off it holds at holdTarget,
// preserving today's reclaim-only behavior; when on it grows
// toward assigned (the ceiling) once utilizationPercent reaches
// GrowUtilizationPercent. The reconcile loop supplies utilizationPercent 0 until a
// measured guest signal exists (RFC milestone 4), so auto-grow stays inert.
func growthTargetBytes(cfg ActiveBallooningConfig, holdTarget, assigned, protectedFloor int64, utilizationPercent int) int64 {
	if !cfg.GrowOnDemandEnabled || utilizationPercent < cfg.GrowUtilizationPercent {
		return clampInt64(holdTarget, protectedFloor, assigned)
	}
	target := assigned
	if target < holdTarget {
		target = holdTarget
	}
	return clampInt64(target, protectedFloor, assigned)
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func clampInt64(v, minV, maxV int64) int64 {
	if v < minV {
		return minV
	}
	if v > maxV {
		return maxV
	}
	return v
}

// mulDivInt64 returns a*b/d computed via a 128-bit intermediate so the product
// does not overflow int64. a, b, and d must be non-negative, d must be positive,
// and a*b/d must fit in int64 (callers here guarantee the quotient is bounded by
// one of the operands).
func mulDivInt64(a, b, d int64) int64 {
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	q, _ := bits.Div64(hi, lo, uint64(d))
	return int64(q)
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
