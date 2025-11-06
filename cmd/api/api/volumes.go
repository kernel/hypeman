package api

import (
	"context"

	"github.com/onkernel/cloud-hypervisor-dataplane/lib/oapi"
)

// ListVolumes lists all volumes
func (s *ApiService) ListVolumes(ctx context.Context, request oapi.ListVolumesRequestObject) (oapi.ListVolumesResponseObject, error) {
	vols, err := s.VolumeManager.ListVolumes(ctx)
	if err != nil {
		return oapi.ListVolumes401JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.ListVolumes200JSONResponse(vols), nil
}

// CreateVolume creates a new volume
func (s *ApiService) CreateVolume(ctx context.Context, request oapi.CreateVolumeRequestObject) (oapi.CreateVolumeResponseObject, error) {
	vol, err := s.VolumeManager.CreateVolume(ctx, *request.Body)
	if err != nil {
		return oapi.CreateVolume400JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.CreateVolume201JSONResponse(*vol), nil
}

// GetVolume gets volume details
func (s *ApiService) GetVolume(ctx context.Context, request oapi.GetVolumeRequestObject) (oapi.GetVolumeResponseObject, error) {
	vol, err := s.VolumeManager.GetVolume(ctx, request.Id)
	if err != nil {
		return oapi.GetVolume404JSONResponse{
			Code:    "not_found",
			Message: err.Error(),
		}, nil
	}
	return oapi.GetVolume200JSONResponse(*vol), nil
}

// DeleteVolume deletes a volume
func (s *ApiService) DeleteVolume(ctx context.Context, request oapi.DeleteVolumeRequestObject) (oapi.DeleteVolumeResponseObject, error) {
	err := s.VolumeManager.DeleteVolume(ctx, request.Id)
	if err != nil {
		return oapi.DeleteVolume404JSONResponse{
			Code:    "not_found",
			Message: err.Error(),
		}, nil
	}
	return oapi.DeleteVolume204Response{}, nil
}

