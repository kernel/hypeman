package api

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
	"os"
	"strconv"

	"github.com/kernel/hypeman/lib/logger"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/volumes"
)

// volumeMultipartForm holds parsed multipart form data for volume creation
type volumeMultipartForm struct {
	Name        string
	SizeGb      int
	ID          *string
	ContentFile *os.File // Temp file containing the archive content
}

// Close cleans up any temp files
func (f *volumeMultipartForm) Close() {
	if f.ContentFile != nil {
		f.ContentFile.Close()
		os.Remove(f.ContentFile.Name())
	}
}

// parseVolumeMultipartForm parses a multipart form for volume creation.
// It buffers the content field to a temp file to handle any field order.
// Caller must call form.Close() to clean up temp files.
func parseVolumeMultipartForm(multipartReader *multipart.Reader) (*volumeMultipartForm, error) {
	form := &volumeMultipartForm{}

	for {
		part, err := multipartReader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			form.Close()
			return nil, &formError{Code: "invalid_form", Message: "failed to parse multipart form: " + err.Error()}
		}

		switch part.FormName() {
		case "name":
			data, err := io.ReadAll(part)
			if err != nil {
				form.Close()
				return nil, &formError{Code: "invalid_field", Message: "failed to read name field"}
			}
			form.Name = string(data)
		case "size_gb":
			data, err := io.ReadAll(part)
			if err != nil {
				form.Close()
				return nil, &formError{Code: "invalid_field", Message: "failed to read size_gb field"}
			}
			sizeGb, err := strconv.Atoi(string(data))
			if err != nil || sizeGb <= 0 {
				form.Close()
				return nil, &formError{Code: "invalid_field", Message: "size_gb must be a positive integer"}
			}
			form.SizeGb = sizeGb
		case "id":
			data, err := io.ReadAll(part)
			if err != nil {
				form.Close()
				return nil, &formError{Code: "invalid_field", Message: "failed to read id field"}
			}
			idStr := string(data)
			if idStr != "" {
				form.ID = &idStr
			}
		case "content":
			// Reject duplicate content fields to prevent temp file leaks
			if form.ContentFile != nil {
				form.Close()
				return nil, &formError{Code: "invalid_form", Message: "duplicate content field"}
			}
			// Buffer content to a temp file to handle any field order
			tempFile, err := os.CreateTemp("", "volume-archive-*.tar.gz")
			if err != nil {
				form.Close()
				return nil, &formError{Code: "internal_error", Message: "failed to create temp file"}
			}
			_, err = io.Copy(tempFile, part)
			if err != nil {
				tempFile.Close()
				os.Remove(tempFile.Name())
				form.Close()
				return nil, &formError{Code: "invalid_field", Message: "failed to read content field"}
			}
			// Seek back to beginning for reading
			_, err = tempFile.Seek(0, 0)
			if err != nil {
				tempFile.Close()
				os.Remove(tempFile.Name())
				form.Close()
				return nil, &formError{Code: "internal_error", Message: "failed to seek temp file"}
			}
			form.ContentFile = tempFile
		}
	}

	return form, nil
}

// formError represents a form parsing error
type formError struct {
	Code    string
	Message string
}

func (e *formError) Error() string {
	return e.Message
}

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

	oapiVols := make([]oapi.Volume, len(domainVols))
	for i, vol := range domainVols {
		oapiVols[i] = volumeToOAPI(vol)
	}

	return oapi.ListVolumes200JSONResponse(oapiVols), nil
}

// CreateVolume creates a new volume
// Supports two modes:
//   - JSON body: Creates an empty volume of the specified size
//   - Multipart form (DEPRECATED): Creates a volume from a tar.gz archive
//     New integrations should use CreateVolumeFromArchive instead
func (s *ApiService) CreateVolume(ctx context.Context, request oapi.CreateVolumeRequestObject) (oapi.CreateVolumeResponseObject, error) {
	log := logger.FromContext(ctx)

	// Handle JSON request (empty volume)
	if request.JSONBody != nil {
		domainReq := volumes.CreateVolumeRequest{
			Name:   request.JSONBody.Name,
			SizeGb: request.JSONBody.SizeGb,
			Id:     request.JSONBody.Id,
		}

		vol, err := s.VolumeManager.CreateVolume(ctx, domainReq)
		if err != nil {
			if errors.Is(err, volumes.ErrAlreadyExists) {
				return oapi.CreateVolume409JSONResponse{
					Code:    "already_exists",
					Message: "volume with this ID already exists",
				}, nil
			}
			log.ErrorContext(ctx, "failed to create volume", "error", err, "name", request.JSONBody.Name)
			return oapi.CreateVolume500JSONResponse{
				Code:    "internal_error",
				Message: "failed to create volume",
			}, nil
		}
		return oapi.CreateVolume201JSONResponse(volumeToOAPI(*vol)), nil
	}

	// Handle multipart request (DEPRECATED - volume with archive content)
	if request.MultipartBody != nil {
		return s.createVolumeFromMultipartDeprecated(ctx, request.MultipartBody)
	}

	return oapi.CreateVolume400JSONResponse{
		Code:    "invalid_request",
		Message: "request body is required",
	}, nil
}

// createVolumeFromMultipartDeprecated handles the deprecated multipart form on POST /volumes
// New integrations should use POST /volumes/from-archive instead
func (s *ApiService) createVolumeFromMultipartDeprecated(ctx context.Context, multipartReader *multipart.Reader) (oapi.CreateVolumeResponseObject, error) {
	log := logger.FromContext(ctx)

	// Parse the multipart form (handles any field order via temp file buffering)
	form, err := parseVolumeMultipartForm(multipartReader)
	if err != nil {
		var formErr *formError
		if errors.As(err, &formErr) {
			if formErr.Code == "internal_error" {
				return oapi.CreateVolume500JSONResponse{
					Code:    formErr.Code,
					Message: formErr.Message,
				}, nil
			}
			return oapi.CreateVolume400JSONResponse{
				Code:    formErr.Code,
				Message: formErr.Message,
			}, nil
		}
		return oapi.CreateVolume500JSONResponse{
			Code:    "internal_error",
			Message: "failed to parse form",
		}, nil
	}
	defer form.Close()

	// Validate required fields after parsing all fields
	if form.Name == "" {
		return oapi.CreateVolume400JSONResponse{
			Code:    "missing_field",
			Message: "name is required",
		}, nil
	}
	if form.SizeGb <= 0 {
		return oapi.CreateVolume400JSONResponse{
			Code:    "missing_field",
			Message: "size_gb is required",
		}, nil
	}
	if form.ContentFile == nil {
		return oapi.CreateVolume400JSONResponse{
			Code:    "missing_file",
			Message: "content file is required for multipart requests",
		}, nil
	}

	// Create the volume from archive
	domainReq := volumes.CreateVolumeFromArchiveRequest{
		Name:   form.Name,
		SizeGb: form.SizeGb,
		Id:     form.ID,
	}

	vol, err := s.VolumeManager.CreateVolumeFromArchive(ctx, domainReq, form.ContentFile)
	if err != nil {
		if errors.Is(err, volumes.ErrArchiveTooLarge) {
			return oapi.CreateVolume400JSONResponse{
				Code:    "archive_too_large",
				Message: err.Error(),
			}, nil
		}
		if errors.Is(err, volumes.ErrAlreadyExists) {
			return oapi.CreateVolume409JSONResponse{
				Code:    "already_exists",
				Message: "volume with this ID already exists",
			}, nil
		}
		log.ErrorContext(ctx, "failed to create volume from archive", "error", err, "name", form.Name)
		return oapi.CreateVolume500JSONResponse{
			Code:    "internal_error",
			Message: "failed to create volume",
		}, nil
	}

	return oapi.CreateVolume201JSONResponse(volumeToOAPI(*vol)), nil
}

// CreateVolumeFromArchive creates a new volume pre-populated with content from a tar.gz archive
func (s *ApiService) CreateVolumeFromArchive(ctx context.Context, request oapi.CreateVolumeFromArchiveRequestObject) (oapi.CreateVolumeFromArchiveResponseObject, error) {
	log := logger.FromContext(ctx)

	if request.Body == nil {
		return oapi.CreateVolumeFromArchive400JSONResponse{
			Code:    "invalid_request",
			Message: "multipart body is required",
		}, nil
	}

	// Parse the multipart form (handles any field order via temp file buffering)
	form, err := parseVolumeMultipartForm(request.Body)
	if err != nil {
		var formErr *formError
		if errors.As(err, &formErr) {
			if formErr.Code == "internal_error" {
				return oapi.CreateVolumeFromArchive500JSONResponse{
					Code:    formErr.Code,
					Message: formErr.Message,
				}, nil
			}
			return oapi.CreateVolumeFromArchive400JSONResponse{
				Code:    formErr.Code,
				Message: formErr.Message,
			}, nil
		}
		return oapi.CreateVolumeFromArchive500JSONResponse{
			Code:    "internal_error",
			Message: "failed to parse form",
		}, nil
	}
	defer form.Close()

	// Validate required fields after parsing all fields
	if form.Name == "" {
		return oapi.CreateVolumeFromArchive400JSONResponse{
			Code:    "missing_field",
			Message: "name is required",
		}, nil
	}
	if form.SizeGb <= 0 {
		return oapi.CreateVolumeFromArchive400JSONResponse{
			Code:    "missing_field",
			Message: "size_gb is required",
		}, nil
	}
	if form.ContentFile == nil {
		return oapi.CreateVolumeFromArchive400JSONResponse{
			Code:    "missing_file",
			Message: "content file is required",
		}, nil
	}

	// Create the volume from archive
	domainReq := volumes.CreateVolumeFromArchiveRequest{
		Name:   form.Name,
		SizeGb: form.SizeGb,
		Id:     form.ID,
	}

	vol, err := s.VolumeManager.CreateVolumeFromArchive(ctx, domainReq, form.ContentFile)
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
		log.ErrorContext(ctx, "failed to create volume from archive", "error", err, "name", form.Name)
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
