package resources

import (
	"context"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/logger"
)

// GPUResourceStatus represents the GPU resource status for the API response.
// Returns nil if no GPU is available on the host.
type GPUResourceStatus struct {
	Mode             string                      `json:"mode"`               // "vgpu" or "passthrough"
	TotalSlots       int                         `json:"total_slots"`        // VFs for vGPU, physical GPUs for passthrough
	UsedSlots        int                         `json:"used_slots"`         // Slots currently in use, including assigned quarantined VFs
	AllocatableSlots int                         `json:"allocatable_slots"`  // Healthy free slots used by admission control
	QuarantinedSlots int                         `json:"quarantined_slots"`  // Quarantined VFs; may overlap UsedSlots
	Profiles         []devices.GPUProfile        `json:"profiles,omitempty"` // vGPU mode only
	Devices          []devices.PassthroughDevice `json:"devices,omitempty"`  // passthrough mode only

	// PlacementDisabledReason is set when AllocatableSlots is 0 because the
	// VF health state could not be read or written, not because the host is
	// full.
	PlacementDisabledReason string `json:"placement_disabled_reason,omitempty"`
}

// GetGPUStatus returns the current GPU resource status and any error that
// prevents determining allocatable vGPU capacity. The status is still
// returned alongside such an error, with PlacementDisabledReason set. It
// returns nil if no GPU is available or the mode is "none".
func GetGPUStatus(ctx context.Context) (*GPUResourceStatus, error) {
	framework, vfs, err := devices.DiscoverVGPU()
	if err != nil {
		// Only report passthrough once vGPU discovery confirms no vGPU
		// framework. On a vGPU host a transient probe failure would otherwise
		// expose the PFs/VFs as available passthrough slots while active vGPU
		// assignments exist.
		logger.FromContext(ctx).WarnContext(ctx, "failed to discover vGPU state", "error", err)
		return nil, nil
	}
	if framework != devices.VGPUFrameworkNone {
		return getVGPUStatus(ctx, framework, vfs)
	}
	return getPassthroughStatus(), nil
}

// getVGPUStatus returns GPU status for vGPU mode (SR-IOV).
func getVGPUStatus(ctx context.Context, framework devices.VGPUFramework, vfs []devices.VirtualFunction) (*GPUResourceStatus, error) {
	usedSlots := 0
	// Count used VFs (those with a vGPU assigned)
	for _, vf := range vfs {
		if vf.HasMdev {
			usedSlots++
		}
	}

	status := &GPUResourceStatus{
		Mode:       string(devices.GPUModeVGPU),
		TotalSlots: len(vfs),
		UsedSlots:  usedSlots,
	}
	// One VF health snapshot serves both the slot counts and the profile
	// listing, so a status read touches the store once.
	availability, err := devices.GetVGPUAvailability(framework, vfs)
	if err != nil {
		status.PlacementDisabledReason = err.Error()
		return status, err
	}
	status.AllocatableSlots = availability.AllocatableSlots
	status.QuarantinedSlots = availability.QuarantinedSlots

	// Get available profiles (reuse VFs to avoid redundant discovery)
	profiles, err := devices.ListGPUProfilesWithVFs(framework, vfs, availability.Quarantined)
	if err != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to list vGPU profiles; reporting none", "framework", framework, "error", err)
		profiles = nil
	}
	status.Profiles = profiles
	return status, nil
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
		Mode:             string(devices.GPUModePassthrough),
		TotalSlots:       len(passthroughDevices),
		UsedSlots:        usedSlots,
		AllocatableSlots: len(passthroughDevices) - usedSlots,
		Devices:          passthroughDevices,
	}
}
