package api

import (
	"context"
	"errors"

	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/oapi"
)

// ListImages lists all images
func (s *ApiService) ListImages(ctx context.Context, request oapi.ListImagesRequestObject) (oapi.ListImagesResponseObject, error) {
	log := logger.FromContext(ctx)

	imgs, err := s.ImageManager.ListImages(ctx)
	if err != nil {
		log.Error("failed to list images", "error", err)
		return oapi.ListImages500JSONResponse{
			Code:    "internal_error",
			Message: "failed to list images",
		}, nil
	}
	return oapi.ListImages200JSONResponse(imgs), nil
}

// CreateImage creates a new image from an OCI reference
func (s *ApiService) CreateImage(ctx context.Context, request oapi.CreateImageRequestObject) (oapi.CreateImageResponseObject, error) {
	log := logger.FromContext(ctx)

	img, err := s.ImageManager.CreateImage(ctx, *request.Body)
	if err != nil {
		switch {
		case errors.Is(err, images.ErrAlreadyExists):
			return oapi.CreateImage400JSONResponse{
				Code:    "already_exists",
				Message: "image already exists",
			}, nil
		default:
			log.Error("failed to create image", "error", err, "name", request.Body.Name)
			return oapi.CreateImage500JSONResponse{
				Code:    "internal_error",
				Message: "failed to create image",
			}, nil
		}
	}
	return oapi.CreateImage202JSONResponse(*img), nil
}

// GetImage gets image details
func (s *ApiService) GetImage(ctx context.Context, request oapi.GetImageRequestObject) (oapi.GetImageResponseObject, error) {
	log := logger.FromContext(ctx)

	img, err := s.ImageManager.GetImage(ctx, request.Id)
	if err != nil {
		switch {
		case errors.Is(err, images.ErrNotFound):
			return oapi.GetImage404JSONResponse{
				Code:    "not_found",
				Message: "image not found",
			}, nil
		default:
			log.Error("failed to get image", "error", err, "id", request.Id)
			return oapi.GetImage500JSONResponse{
				Code:    "internal_error",
				Message: "failed to get image",
			}, nil
		}
	}
	return oapi.GetImage200JSONResponse(*img), nil
}

// GetImageProgress streams build progress via SSE
func (s *ApiService) GetImageProgress(ctx context.Context, request oapi.GetImageProgressRequestObject) (oapi.GetImageProgressResponseObject, error) {
	log := logger.FromContext(ctx)

	progressChan, err := s.ImageManager.GetProgress(ctx, request.Id)
	if err != nil {
		switch {
		case errors.Is(err, images.ErrNotFound):
			return oapi.GetImageProgress404JSONResponse{
				Code:    "not_found",
				Message: "image not found",
			}, nil
		default:
			log.Error("failed to get progress", "error", err, "id", request.Id)
			return oapi.GetImageProgress500JSONResponse{
				Code:    "internal_error",
				Message: "failed to get progress",
			}, nil
		}
	}

	// Return SSE stream (uses helper from progress.go)
	return oapi.GetImageProgress200TexteventStreamResponse{
		Body: images.ToSSEReader(progressChan),
	}, nil
}

// DeleteImage deletes an image
func (s *ApiService) DeleteImage(ctx context.Context, request oapi.DeleteImageRequestObject) (oapi.DeleteImageResponseObject, error) {
	log := logger.FromContext(ctx)

	err := s.ImageManager.DeleteImage(ctx, request.Id)
	if err != nil {
		switch {
		case errors.Is(err, images.ErrNotFound):
			return oapi.DeleteImage404JSONResponse{
				Code:    "not_found",
				Message: "image not found",
			}, nil
		default:
			log.Error("failed to delete image", "error", err, "id", request.Id)
			return oapi.DeleteImage500JSONResponse{
				Code:    "internal_error",
				Message: "failed to delete image",
			}, nil
		}
	}
	return oapi.DeleteImage204Response{}, nil
}

