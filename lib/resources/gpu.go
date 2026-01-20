package resources

import (
	"github.com/kernel/hypeman/lib/devices"
)

// GPUResourceStatus represents the GPU resource status for the API response.
// Returns nil if no GPU is available on the host.
type GPUResourceStatus struct {
	Mode       string                      `json:"mode"`               // "vgpu" or "passthrough"
	TotalSlots int                         `json:"total_slots"`        // VFs for vGPU, physical GPUs for passthrough
	UsedSlots  int                         `json:"used_slots"`         // Slots currently in use
	Profiles   []devices.GPUProfile        `json:"profiles,omitempty"` // vGPU mode only
	Devices    []devices.PassthroughDevice `json:"devices,omitempty"`  // passthrough mode only
}

// GetGPUStatus returns the current GPU resource status.
// Returns nil if no GPU is available or the mode is "none".
func GetGPUStatus() *GPUResourceStatus {
	mode := devices.DetectHostGPUMode()
	if mode == devices.GPUModeNone {
		return nil
	}

	switch mode {
	case devices.GPUModeVGPU:
		return getVGPUStatus()
	case devices.GPUModePassthrough:
		return getPassthroughStatus()
	default:
		return nil
	}
}

// getVGPUStatus returns GPU status for vGPU mode (SR-IOV + mdev).
func getVGPUStatus() *GPUResourceStatus {
	vfs, err := devices.DiscoverVFs()
	if err != nil || len(vfs) == 0 {
		return nil
	}

	// Count used VFs (those with mdevs)
	usedSlots := 0
	for _, vf := range vfs {
		if vf.HasMdev {
			usedSlots++
		}
	}

	// Get available profiles (reuse VFs to avoid redundant discovery)
	profiles, err := devices.ListGPUProfilesWithVFs(vfs)
	if err != nil {
		profiles = nil
	}

	return &GPUResourceStatus{
		Mode:       string(devices.GPUModeVGPU),
		TotalSlots: len(vfs),
		UsedSlots:  usedSlots,
		Profiles:   profiles,
	}
}

// getPassthroughStatus returns GPU status for whole-GPU passthrough mode.
func getPassthroughStatus() *GPUResourceStatus {
	available, err := devices.DiscoverAvailableDevices()
	if err != nil || len(available) == 0 {
		return nil
	}

	// Filter to GPUs only and build passthrough device list
	var passthroughDevices []devices.PassthroughDevice
	for _, dev := range available {
		// NVIDIA vendor ID is 0x10de
		if dev.VendorID == "10de" {
			passthroughDevices = append(passthroughDevices, devices.PassthroughDevice{
				Name:      dev.DeviceName,
				Available: dev.CurrentDriver == nil || *dev.CurrentDriver != "vfio-pci",
			})
		}
	}

	if len(passthroughDevices) == 0 {
		return nil
	}

	// Count used (those bound to vfio-pci, likely attached to a VM)
	usedSlots := 0
	for _, dev := range passthroughDevices {
		if !dev.Available {
			usedSlots++
		}
	}

	return &GPUResourceStatus{
		Mode:       string(devices.GPUModePassthrough),
		TotalSlots: len(passthroughDevices),
		UsedSlots:  usedSlots,
		Devices:    passthroughDevices,
	}
}
