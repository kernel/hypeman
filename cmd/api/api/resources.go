package api

import (
	"context"

	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/resources"
)

// GetResources returns host resource capacity and allocations
func (s *ApiService) GetResources(ctx context.Context, _ oapi.GetResourcesRequestObject) (oapi.GetResourcesResponseObject, error) {
	if s.ResourceManager == nil {
		return oapi.GetResources500JSONResponse{
			Code:    "internal_error",
			Message: "Resource manager not initialized",
		}, nil
	}

	status, err := s.ResourceManager.GetFullStatus(ctx)
	if err != nil {
		return oapi.GetResources500JSONResponse{
			Code:    "internal_error",
			Message: err.Error(),
		}, nil
	}

	// Convert to API response
	resp := oapi.Resources{
		Cpu:         convertResourceStatus(status.CPU),
		Memory:      convertResourceStatus(status.Memory),
		Disk:        convertResourceStatus(status.Disk),
		Network:     convertResourceStatus(status.Network),
		Allocations: make([]oapi.ResourceAllocation, 0, len(status.Allocations)),
	}

	// Add disk breakdown if available
	if status.DiskDetail != nil {
		resp.DiskBreakdown = &oapi.DiskBreakdown{
			ImagesBytes:   &status.DiskDetail.Images,
			OciCacheBytes: &status.DiskDetail.OCICache,
			VolumesBytes:  &status.DiskDetail.Volumes,
			OverlaysBytes: &status.DiskDetail.Overlays,
		}
	}

	// Add per-instance allocations
	for _, alloc := range status.Allocations {
		resp.Allocations = append(resp.Allocations, oapi.ResourceAllocation{
			InstanceId:         &alloc.InstanceID,
			InstanceName:       &alloc.InstanceName,
			Cpu:                &alloc.CPU,
			MemoryBytes:        &alloc.MemoryBytes,
			DiskBytes:          &alloc.DiskBytes,
			NetworkDownloadBps: &alloc.NetworkDownloadBps,
			NetworkUploadBps:   &alloc.NetworkUploadBps,
		})
	}

	// Add GPU status if available
	if status.GPU != nil {
		gpuStatus := convertGPUResourceStatus(status.GPU)
		resp.Gpu = &gpuStatus
	}

	return oapi.GetResources200JSONResponse(resp), nil
}

func convertResourceStatus(rs resources.ResourceStatus) oapi.ResourceStatus {
	var source *string
	if rs.Source != "" {
		s := string(rs.Source)
		source = &s
	}
	return oapi.ResourceStatus{
		Type:           string(rs.Type),
		Capacity:       rs.Capacity,
		EffectiveLimit: rs.EffectiveLimit,
		Allocated:      rs.Allocated,
		Available:      rs.Available,
		OversubRatio:   rs.OversubRatio,
		Source:         source,
	}
}

func convertGPUResourceStatus(gs *resources.GPUResourceStatus) oapi.GPUResourceStatus {
	result := oapi.GPUResourceStatus{
		Mode:       oapi.GPUResourceStatusMode(gs.Mode),
		TotalSlots: gs.TotalSlots,
		UsedSlots:  gs.UsedSlots,
	}

	// Convert profiles (vGPU mode)
	if len(gs.Profiles) > 0 {
		profiles := make([]oapi.GPUProfile, len(gs.Profiles))
		for i, p := range gs.Profiles {
			profiles[i] = oapi.GPUProfile{
				Name:          p.Name,
				FramebufferMb: p.FramebufferMB,
				Available:     p.Available,
			}
		}
		result.Profiles = &profiles
	}

	// Convert devices (passthrough mode)
	if len(gs.Devices) > 0 {
		devices := make([]oapi.PassthroughDevice, len(gs.Devices))
		for i, d := range gs.Devices {
			devices[i] = oapi.PassthroughDevice{
				Name:      d.Name,
				Available: d.Available,
			}
		}
		result.Devices = &devices
	}

	return result
}
