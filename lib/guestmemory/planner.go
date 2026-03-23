package guestmemory

import "github.com/kernel/hypeman/lib/hypervisor"

type candidateState struct {
	vm                      BalloonVM
	hv                      hypervisor.Hypervisor
	currentTargetGuestBytes int64
	protectedFloorBytes     int64
	maxReclaimBytes         int64
}

func planGuestTargets(cfg ActiveBallooningConfig, candidates []candidateState, totalReclaim int64) map[string]int64 {
	targets := make(map[string]int64, len(candidates))
	if len(candidates) == 0 {
		return targets
	}

	var totalHeadroom int64
	for _, candidate := range candidates {
		totalHeadroom += candidate.maxReclaimBytes
		targets[candidate.vm.ID] = candidate.vm.AssignedMemoryBytes
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
		reclaim := (totalReclaim * candidate.maxReclaimBytes) / totalHeadroom
		if reclaim > candidate.maxReclaimBytes {
			reclaim = candidate.maxReclaimBytes
		}
		targets[candidate.vm.ID] = candidate.vm.AssignedMemoryBytes - reclaim
		remainder -= reclaim
	}

	for _, candidate := range candidates {
		if remainder <= 0 {
			break
		}
		currentReclaim := candidate.vm.AssignedMemoryBytes - targets[candidate.vm.ID]
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

func protectedFloorBytes(cfg ActiveBallooningConfig, assigned int64) int64 {
	percentFloor := (assigned * int64(cfg.ProtectedFloorPercent)) / 100
	return maxInt64(cfg.ProtectedFloorMinBytes, percentFloor)
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
