package api

import (
	"context"
	"errors"
	"io"
	"strconv"

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
// The id parameter can be either a volume ID or name
func (s *ApiService) GetVolume(ctx context.Context, request oapi.GetVolumeRequestObject) (oapi.GetVolumeResponseObject, error) {
	log := logger.FromContext(ctx)

	// Try lookup by ID first
	vol, err := s.VolumeManager.GetVolume(ctx, request.Id)
	if errors.Is(err, volumes.ErrNotFound) {
		// Try lookup by name
		vol, err = s.VolumeManager.GetVolumeByName(ctx, request.Id)
	}

	if err != nil {
		switch {
		case errors.Is(err, volumes.ErrNotFound):
			return oapi.GetVolume404JSONResponse{
				Code:    "not_found",
				Message: "volume not found",
			}, nil
		case errors.Is(err, volumes.ErrAmbiguousName):
			return oapi.GetVolume404JSONResponse{
				Code:    "ambiguous_name",
				Message: "multiple volumes have this name, use volume ID instead",
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
// The id parameter can be either a volume ID or name
func (s *ApiService) DeleteVolume(ctx context.Context, request oapi.DeleteVolumeRequestObject) (oapi.DeleteVolumeResponseObject, error) {
	log := logger.FromContext(ctx)

	// Resolve ID - try direct ID first, then name lookup
	volumeID := request.Id
	_, err := s.VolumeManager.GetVolume(ctx, request.Id)
	if errors.Is(err, volumes.ErrNotFound) {
		// Try lookup by name
		vol, nameErr := s.VolumeManager.GetVolumeByName(ctx, request.Id)
		if nameErr == nil {
			volumeID = vol.Id
		} else if errors.Is(nameErr, volumes.ErrAmbiguousName) {
			return oapi.DeleteVolume404JSONResponse{
				Code:    "ambiguous_name",
				Message: "multiple volumes have this name, use volume ID instead",
			}, nil
		}
		// If name lookup also fails with ErrNotFound, we'll proceed with original ID
		// and let DeleteVolume return the proper 404
	}

	err = s.VolumeManager.DeleteVolume(ctx, volumeID)
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

	// Convert attachments
	if len(vol.Attachments) > 0 {
		attachments := make([]oapi.VolumeAttachmentInfo, len(vol.Attachments))
		for i, att := range vol.Attachments {
			attachments[i] = oapi.VolumeAttachmentInfo{
				InstanceId: att.InstanceID,
				MountPath:  att.MountPath,
				Readonly:   att.Readonly,
			}
		}
		oapiVol.Attachments = &attachments
	}

	return oapiVol
}

// CreateVolumeFromArchive creates a volume pre-populated with content from a tar.gz archive
func (s *ApiService) CreateVolumeFromArchive(ctx context.Context, request oapi.CreateVolumeFromArchiveRequestObject) (oapi.CreateVolumeFromArchiveResponseObject, error) {
	log := logger.FromContext(ctx)

	// Read the multipart form data from the request body
	var name string
	var sizeGb int
	var id *string
	var archiveReader io.Reader

	for {
		part, err := request.Body.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return oapi.CreateVolumeFromArchive400JSONResponse{
				Code:    "invalid_form",
				Message: "failed to parse multipart form: " + err.Error(),
			}, nil
		}

		switch part.FormName() {
		case "name":
			data, err := io.ReadAll(part)
			if err != nil {
				return oapi.CreateVolumeFromArchive400JSONResponse{
					Code:    "invalid_field",
					Message: "failed to read name field",
				}, nil
			}
			name = string(data)
		case "size_gb":
			data, err := io.ReadAll(part)
			if err != nil {
				return oapi.CreateVolumeFromArchive400JSONResponse{
					Code:    "invalid_field",
					Message: "failed to read size_gb field",
				}, nil
			}
			sizeGb, err = strconv.Atoi(string(data))
			if err != nil || sizeGb <= 0 {
				return oapi.CreateVolumeFromArchive400JSONResponse{
					Code:    "invalid_field",
					Message: "size_gb must be a positive integer",
				}, nil
			}
		case "id":
			data, err := io.ReadAll(part)
			if err != nil {
				return oapi.CreateVolumeFromArchive400JSONResponse{
					Code:    "invalid_field",
					Message: "failed to read id field",
				}, nil
			}
			idStr := string(data)
			if idStr != "" {
				id = &idStr
			}
		case "content":
			archiveReader = part
			// Process the archive immediately while we have the reader
			if name == "" {
				return oapi.CreateVolumeFromArchive400JSONResponse{
					Code:    "missing_field",
					Message: "name is required",
				}, nil
			}
			if sizeGb <= 0 {
				return oapi.CreateVolumeFromArchive400JSONResponse{
					Code:    "missing_field",
					Message: "size_gb is required",
				}, nil
			}

			// Create the volume
			domainReq := volumes.CreateVolumeFromArchiveRequest{
				Name:   name,
				SizeGb: sizeGb,
				Id:     id,
			}

			vol, err := s.VolumeManager.CreateVolumeFromArchive(ctx, domainReq, archiveReader)
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
				log.Error("failed to create volume from archive", "error", err, "name", name)
				return oapi.CreateVolumeFromArchive500JSONResponse{
					Code:    "internal_error",
					Message: "failed to create volume",
				}, nil
			}

			return oapi.CreateVolumeFromArchive201JSONResponse(volumeToOAPI(*vol)), nil
		}
	}

	// If we get here without processing content, it means content was not provided
	if archiveReader == nil {
		return oapi.CreateVolumeFromArchive400JSONResponse{
			Code:    "missing_file",
			Message: "content file is required",
		}, nil
	}

	// Should not reach here, but return error just in case
	return oapi.CreateVolumeFromArchive500JSONResponse{
		Code:    "internal_error",
		Message: "unexpected error processing request",
	}, nil
}

