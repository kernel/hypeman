package devices

// DetectHostGPUMode determines the host's GPU configuration mode.
//
// Returns:
//   - GPUModeVGPU if an mdev or vendor VFIO vGPU framework is available
//   - GPUModePassthrough if NVIDIA GPUs are available for VFIO passthrough
//   - GPUModeNone if no GPUs are available
//
// Note: A host is configured for either vGPU or passthrough, not both,
// because the host driver determines which mode is available.
func DetectHostGPUMode() GPUMode {
	if DetectVGPUFramework() != VGPUFrameworkNone {
		return GPUModeVGPU
	}

	// Check for passthrough mode (physical GPUs available)
	gpus, err := DiscoverAvailableDevices()
	if err == nil && len(gpus) > 0 {
		return GPUModePassthrough
	}

	return GPUModeNone
}
