package api

import (
	"context"
	"errors"

	"github.com/kernel/hypeman/lib/logger"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/tags"
	"github.com/kernel/hypeman/lib/volumes"
)

// ListVolumes lists all volumes
func (s *ApiService) ListVolumes(ctx context.Context, request oapi.ListVolumesRequestObject) (oapi.ListVolumesResponseObject, error) {
	log := logger.FromContext(ctx)

	domainVols, err := s.VolumeManager.ListVolumes(ctx)
	if err != nil {
		log.ErrorContext(ctx, "failed to list volumes", "error", err)
		return oapi.ListVolumes500JSONResponse{
			Code:    "internal_error",
			Message: "failed to list volumes",
		}, nil
	}

	oapiVols := make([]oapi.Volume, 0, len(domainVols))
	for _, vol := range domainVols {
		if !matchesMetadataFilter(vol.Metadata, request.Params.Metadata) {
			continue
		}
		oapiVols = append(oapiVols, volumeToOAPI(vol))
	}

	return oapi.ListVolumes200JSONResponse(oapiVols), nil
}

// CreateVolume creates a new empty volume of the specified size
func (s *ApiService) CreateVolume(ctx context.Context, request oapi.CreateVolumeRequestObject) (oapi.CreateVolumeResponseObject, error) {
	log := logger.FromContext(ctx)

	if request.Body == nil {
		return oapi.CreateVolume400JSONResponse{
			Code:    "invalid_request",
			Message: "request body is required",
		}, nil
	}

	domainReq := volumes.CreateVolumeRequest{
		Name:     request.Body.Name,
		SizeGb:   request.Body.SizeGb,
		Id:       request.Body.Id,
		Metadata: toMapMetadata(request.Body.Metadata),
	}

	vol, err := s.VolumeManager.CreateVolume(ctx, domainReq)
	if err != nil {
		if errors.Is(err, volumes.ErrAlreadyExists) {
			return oapi.CreateVolume409JSONResponse{
				Code:    "already_exists",
				Message: "volume with this ID already exists",
			}, nil
		}
		if errors.Is(err, tags.ErrInvalidMetadata) {
			return oapi.CreateVolume400JSONResponse{
				Code:    "invalid_request",
				Message: err.Error(),
			}, nil
		}
		log.ErrorContext(ctx, "failed to create volume", "error", err, "name", request.Body.Name)
		return oapi.CreateVolume500JSONResponse{
			Code:    "internal_error",
			Message: "failed to create volume",
		}, nil
	}
	return oapi.CreateVolume201JSONResponse(volumeToOAPI(*vol)), nil
}

// CreateVolumeFromArchive creates a new volume pre-populated with content from a tar.gz archive
// The archive is streamed directly into the volume without intermediate buffering
func (s *ApiService) CreateVolumeFromArchive(ctx context.Context, request oapi.CreateVolumeFromArchiveRequestObject) (oapi.CreateVolumeFromArchiveResponseObject, error) {
	log := logger.FromContext(ctx)

	// Validate required parameters
	if request.Params.Name == "" {
		return oapi.CreateVolumeFromArchive400JSONResponse{
			Code:    "missing_field",
			Message: "name query parameter is required",
		}, nil
	}
	if request.Params.SizeGb <= 0 {
		return oapi.CreateVolumeFromArchive400JSONResponse{
			Code:    "invalid_field",
			Message: "size_gb must be a positive integer",
		}, nil
	}
	// Note: request.Body is never nil in Go's net/http (empty body = http.NoBody)
	// Empty/invalid archives will fail with a clear gzip error downstream

	// Create the volume from archive - stream directly without buffering
	domainReq := volumes.CreateVolumeFromArchiveRequest{
		Name:     request.Params.Name,
		SizeGb:   request.Params.SizeGb,
		Id:       request.Params.Id,
		Metadata: toMapMetadata(request.Params.Metadata),
	}

	vol, err := s.VolumeManager.CreateVolumeFromArchive(ctx, domainReq, request.Body)
	if err != nil {
		if errors.Is(err, volumes.ErrArchiveTooLarge) {
			return oapi.CreateVolumeFromArchive400JSONResponse{
				Code:    "archive_too_large",
				Message: err.Error(),
			}, nil
		}
		if errors.Is(err, volumes.ErrAlreadyExists) {
			return oapi.CreateVolumeFromArchive409JSONResponse{
				Code:    "already_exists",
				Message: "volume with this ID already exists",
			}, nil
		}
		if errors.Is(err, tags.ErrInvalidMetadata) {
			return oapi.CreateVolumeFromArchive400JSONResponse{
				Code:    "invalid_request",
				Message: err.Error(),
			}, nil
		}
		log.ErrorContext(ctx, "failed to create volume from archive", "error", err, "name", request.Params.Name)
		return oapi.CreateVolumeFromArchive500JSONResponse{
			Code:    "internal_error",
			Message: "failed to create volume",
		}, nil
	}

	return oapi.CreateVolumeFromArchive201JSONResponse(volumeToOAPI(*vol)), nil
}

// GetVolume gets volume details
// The id parameter can be either a volume ID or name
// Note: Resolution is handled by ResolveResource middleware
func (s *ApiService) GetVolume(ctx context.Context, request oapi.GetVolumeRequestObject) (oapi.GetVolumeResponseObject, error) {
	vol := mw.GetResolvedVolume[volumes.Volume](ctx)
	if vol == nil {
		return oapi.GetVolume500JSONResponse{
			Code:    "internal_error",
			Message: "resource not resolved",
		}, nil
	}
	return oapi.GetVolume200JSONResponse(volumeToOAPI(*vol)), nil
}

// DeleteVolume deletes a volume
// The id parameter can be either a volume ID or name
// Note: Resolution is handled by ResolveResource middleware
func (s *ApiService) DeleteVolume(ctx context.Context, request oapi.DeleteVolumeRequestObject) (oapi.DeleteVolumeResponseObject, error) {
	vol := mw.GetResolvedVolume[volumes.Volume](ctx)
	if vol == nil {
		return oapi.DeleteVolume500JSONResponse{
			Code:    "internal_error",
			Message: "resource not resolved",
		}, nil
	}
	log := logger.FromContext(ctx)

	err := s.VolumeManager.DeleteVolume(ctx, vol.Id)
	if err != nil {
		switch {
		case errors.Is(err, volumes.ErrInUse):
			return oapi.DeleteVolume409JSONResponse{
				Code:    "conflict",
				Message: "volume is in use by an instance",
			}, nil
		default:
			log.ErrorContext(ctx, "failed to delete volume", "error", err)
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
		Metadata:  toOAPIMetadata(vol.Metadata),
		CreatedAt: vol.CreatedAt,
	}

	// Convert attachments
	if len(vol.Attachments) > 0 {
		attachments := make([]oapi.VolumeAttachment, len(vol.Attachments))
		for i, att := range vol.Attachments {
			attachments[i] = oapi.VolumeAttachment{
				InstanceId: att.InstanceID,
				MountPath:  att.MountPath,
				Readonly:   att.Readonly,
			}
		}
		oapiVol.Attachments = &attachments
	}

	return oapiVol
}
