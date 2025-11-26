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

	domainVols, err := s.VolumeManager.ListVolumes(ctx)
	if err != nil {
		log.Error("failed to list volumes", "error", err)
		return oapi.ListVolumes500JSONResponse{
			Code:    "internal_error",
			Message: "failed to list volumes",
		}, nil
	}
	
	oapiVols := make([]oapi.Volume, len(domainVols))
	for i, vol := range domainVols {
		oapiVols[i] = volumeToOAPI(vol)
	}
	
	return oapi.ListVolumes200JSONResponse(oapiVols), nil
}

// CreateVolume creates a new volume
func (s *ApiService) CreateVolume(ctx context.Context, request oapi.CreateVolumeRequestObject) (oapi.CreateVolumeResponseObject, error) {
	log := logger.FromContext(ctx)

	domainReq := volumes.CreateVolumeRequest{
		Name:   request.Body.Name,
		SizeGb: request.Body.SizeGb,
		Id:     request.Body.Id,
	}

	vol, err := s.VolumeManager.CreateVolume(ctx, domainReq)
	if err != nil {
		log.Error("failed to create volume", "error", err, "name", request.Body.Name)
		return oapi.CreateVolume500JSONResponse{
			Code:    "internal_error",
			Message: "failed to create volume",
		}, nil
	}
	return oapi.CreateVolume201JSONResponse(volumeToOAPI(*vol)), nil
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
	return oapi.GetVolume200JSONResponse(volumeToOAPI(*vol)), nil
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

func volumeToOAPI(vol volumes.Volume) oapi.Volume {
	oapiVol := oapi.Volume{
		Id:        vol.Id,
		Name:      vol.Name,
		SizeGb:    vol.SizeGb,
		CreatedAt: vol.CreatedAt,
	}
	if vol.AttachedTo != nil {
		oapiVol.AttachedTo = vol.AttachedTo
	}
	if vol.MountPath != nil {
		oapiVol.MountPath = vol.MountPath
	}
	return oapiVol
}

