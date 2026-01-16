package devices

import (
	"os"
)

// DetectHostGPUMode determines the host's GPU configuration mode.
//
// Returns:
//   - GPUModeVGPU if /sys/class/mdev_bus has entries (SR-IOV VFs present)
//   - GPUModePassthrough if NVIDIA GPUs are available for VFIO passthrough
//   - GPUModeNone if no GPUs are available
//
// Note: A host is configured for either vGPU or passthrough, not both,
// because the host driver determines which mode is available.
func DetectHostGPUMode() GPUMode {
	// Check for vGPU mode first (SR-IOV VFs present)
	entries, err := os.ReadDir("/sys/class/mdev_bus")
	if err == nil && len(entries) > 0 {
		return GPUModeVGPU
	}

	// Check for passthrough mode (physical GPUs available)
	gpus, err := DiscoverAvailableDevices()
	if err == nil && len(gpus) > 0 {
		return GPUModePassthrough
	}

	return GPUModeNone
}
