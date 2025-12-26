package api

import (
	"context"

	"github.com/onkernel/hypeman/lib/oapi"
	"github.com/onkernel/hypeman/lib/resources"
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

	return oapi.GetResources200JSONResponse(resp), nil
}

func convertResourceStatus(rs resources.ResourceStatus) oapi.ResourceStatus {
	return oapi.ResourceStatus{
		Type:           string(rs.Type),
		Capacity:       rs.Capacity,
		EffectiveLimit: rs.EffectiveLimit,
		Allocated:      rs.Allocated,
		Available:      rs.Available,
		OversubRatio:   rs.OversubRatio,
		Source:         &rs.Source,
	}
}
