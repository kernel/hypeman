package api

import (
	"context"
	"errors"

	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/oapi"
	"github.com/onkernel/hypeman/lib/volumes"
)

// ListVolumes lists all volumes
func (s *ApiService) ListVolumes(ctx context.Context, request oapi.ListVolumesRequestObject) (oapi.ListVolumesResponseObject, error) {
	log := logger.FromContext(ctx)

	vols, err := s.VolumeManager.ListVolumes(ctx)
	if err != nil {
		log.Error("failed to list volumes", "error", err)
		return oapi.ListVolumes500JSONResponse{
			Code:    "internal_error",
			Message: "failed to list volumes",
		}, nil
	}
	return oapi.ListVolumes200JSONResponse(vols), nil
}

// CreateVolume creates a new volume
func (s *ApiService) CreateVolume(ctx context.Context, request oapi.CreateVolumeRequestObject) (oapi.CreateVolumeResponseObject, error) {
	log := logger.FromContext(ctx)

	vol, err := s.VolumeManager.CreateVolume(ctx, *request.Body)
	if err != nil {
		log.Error("failed to create volume", "error", err, "name", request.Body.Name)
		return oapi.CreateVolume500JSONResponse{
			Code:    "internal_error",
			Message: "failed to create volume",
		}, nil
	}
	return oapi.CreateVolume201JSONResponse(*vol), nil
}

// GetVolume gets volume details
func (s *ApiService) GetVolume(ctx context.Context, request oapi.GetVolumeRequestObject) (oapi.GetVolumeResponseObject, error) {
	log := logger.FromContext(ctx)

	vol, err := s.VolumeManager.GetVolume(ctx, request.Id)
	if err != nil {
		switch {
		case errors.Is(err, volumes.ErrNotFound):
			return oapi.GetVolume404JSONResponse{
				Code:    "not_found",
				Message: "volume not found",
			}, nil
		default:
			log.Error("failed to get volume", "error", err, "id", request.Id)
			return oapi.GetVolume500JSONResponse{
				Code:    "internal_error",
				Message: "failed to get volume",
			}, nil
		}
	}
	return oapi.GetVolume200JSONResponse(*vol), nil
}

// DeleteVolume deletes a volume
func (s *ApiService) DeleteVolume(ctx context.Context, request oapi.DeleteVolumeRequestObject) (oapi.DeleteVolumeResponseObject, error) {
	log := logger.FromContext(ctx)

	err := s.VolumeManager.DeleteVolume(ctx, request.Id)
	if err != nil {
		switch {
		case errors.Is(err, volumes.ErrNotFound):
			return oapi.DeleteVolume404JSONResponse{
				Code:    "not_found",
				Message: "volume not found",
			}, nil
		case errors.Is(err, volumes.ErrInUse):
			return oapi.DeleteVolume409JSONResponse{
				Code:    "conflict",
				Message: "volume is in use by an instance",
			}, nil
		default:
			log.Error("failed to delete volume", "error", err, "id", request.Id)
			return oapi.DeleteVolume500JSONResponse{
				Code:    "internal_error",
				Message: "failed to delete volume",
			}, nil
		}
	}
	return oapi.DeleteVolume204Response{}, nil
}

