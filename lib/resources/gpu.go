package resources

import (
	"context"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/logger"
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
func GetGPUStatus(ctx context.Context) *GPUResourceStatus {
	framework, vfs, err := devices.DiscoverVGPU()
	if err != nil {
		// A failed vGPU probe must not hide passthrough GPUs from status reporting.
		logger.FromContext(ctx).WarnContext(ctx, "failed to discover vGPU state", "error", err)
		return getPassthroughStatus()
	}
	if framework != devices.VGPUFrameworkNone {
		return getVGPUStatus(ctx, framework, vfs)
	}
	return getPassthroughStatus()
}

// getVGPUStatus returns GPU status for vGPU mode (SR-IOV).
func getVGPUStatus(ctx context.Context, framework devices.VGPUFramework, vfs []devices.VirtualFunction) *GPUResourceStatus {
	usedSlots := 0
	// Count used VFs (those with a vGPU assigned)
	for _, vf := range vfs {
		if vf.Allocated {
			usedSlots++
		}
	}

	// Get available profiles (reuse VFs to avoid redundant discovery)
	profiles, err := devices.ListGPUProfilesWithVFs(framework, vfs)
	if err != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to list vGPU profiles; reporting none", "framework", framework, "error", err)
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
