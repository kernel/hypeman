package api

import (
	"context"
	"errors"

	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/logger"
	mw "github.com/onkernel/hypeman/lib/middleware"
	"github.com/onkernel/hypeman/lib/oapi"
)

func (s *ApiService) ListImages(ctx context.Context, request oapi.ListImagesRequestObject) (oapi.ListImagesResponseObject, error) {
	log := logger.FromContext(ctx)

	domainImages, err := s.ImageManager.ListImages(ctx)
	if err != nil {
		log.ErrorContext(ctx, "failed to list images", "error", err)
		return oapi.ListImages500JSONResponse{
			Code:    "internal_error",
			Message: "failed to list images",
		}, nil
	}

	oapiImages := make([]oapi.Image, len(domainImages))
	for i, img := range domainImages {
		oapiImages[i] = imageToOAPI(img)
	}

	return oapi.ListImages200JSONResponse(oapiImages), nil
}

func (s *ApiService) CreateImage(ctx context.Context, request oapi.CreateImageRequestObject) (oapi.CreateImageResponseObject, error) {
	log := logger.FromContext(ctx)

	domainReq := images.CreateImageRequest{
		Name: request.Body.Name,
	}

	img, err := s.ImageManager.CreateImage(ctx, domainReq)
	if err != nil {
		switch {
		case errors.Is(err, images.ErrInvalidName):
			return oapi.CreateImage400JSONResponse{
				Code:    "invalid_name",
				Message: err.Error(),
			}, nil
		case errors.Is(err, images.ErrNotFound):
			return oapi.CreateImage404JSONResponse{
				Code:    "not_found",
				Message: "image not found",
			}, nil
		default:
			log.ErrorContext(ctx, "failed to create image", "error", err, "name", request.Body.Name)
			return oapi.CreateImage500JSONResponse{
				Code:    "internal_error",
				Message: "failed to create image",
			}, nil
		}
	}
	return oapi.CreateImage202JSONResponse(imageToOAPI(*img)), nil
}

// GetImage gets image details by name
// Note: Resolution is handled by ResolveResource middleware
func (s *ApiService) GetImage(ctx context.Context, request oapi.GetImageRequestObject) (oapi.GetImageResponseObject, error) {
	img := mw.GetResolvedImage[images.Image](ctx)
	if img == nil {
		return oapi.GetImage500JSONResponse{
			Code:    "internal_error",
			Message: "resource not resolved",
		}, nil
	}
	return oapi.GetImage200JSONResponse(imageToOAPI(*img)), nil
}

// DeleteImage deletes an image by name
// Note: Resolution is handled by ResolveResource middleware
func (s *ApiService) DeleteImage(ctx context.Context, request oapi.DeleteImageRequestObject) (oapi.DeleteImageResponseObject, error) {
	img := mw.GetResolvedImage[images.Image](ctx)
	if img == nil {
		return oapi.DeleteImage500JSONResponse{
			Code:    "internal_error",
			Message: "resource not resolved",
		}, nil
	}
	log := logger.FromContext(ctx)

	err := s.ImageManager.DeleteImage(ctx, img.Name)
	if err != nil {
		log.ErrorContext(ctx, "failed to delete image", "error", err)
		return oapi.DeleteImage500JSONResponse{
			Code:    "internal_error",
			Message: "failed to delete image",
		}, nil
	}
	return oapi.DeleteImage204Response{}, nil
}

func imageToOAPI(img images.Image) oapi.Image {
	oapiImg := oapi.Image{
		Name:          img.Name,
		Digest:        img.Digest,
		Status:        oapi.ImageStatus(img.Status),
		QueuePosition: img.QueuePosition,
		Error:         img.Error,
		SizeBytes:     img.SizeBytes,
		CreatedAt:     img.CreatedAt,
	}

	if len(img.Entrypoint) > 0 {
		oapiImg.Entrypoint = &img.Entrypoint
	}
	if len(img.Cmd) > 0 {
		oapiImg.Cmd = &img.Cmd
	}
	if len(img.Env) > 0 {
		oapiImg.Env = &img.Env
	}
	if img.WorkingDir != "" {
		oapiImg.WorkingDir = &img.WorkingDir
	}

	return oapiImg
}
