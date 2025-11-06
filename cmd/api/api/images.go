package api

import (
	"context"

	"github.com/onkernel/cloud-hypervisor-dataplane/lib/oapi"
)

// ListImages lists all images
func (s *ApiService) ListImages(ctx context.Context, request oapi.ListImagesRequestObject) (oapi.ListImagesResponseObject, error) {
	imgs, err := s.ImageManager.ListImages(ctx)
	if err != nil {
		return oapi.ListImages401JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.ListImages200JSONResponse(imgs), nil
}

// CreateImage creates a new image from an OCI reference
func (s *ApiService) CreateImage(ctx context.Context, request oapi.CreateImageRequestObject) (oapi.CreateImageResponseObject, error) {
	img, err := s.ImageManager.CreateImage(ctx, *request.Body)
	if err != nil {
		return oapi.CreateImage400JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.CreateImage201JSONResponse(*img), nil
}

// GetImage gets image details
func (s *ApiService) GetImage(ctx context.Context, request oapi.GetImageRequestObject) (oapi.GetImageResponseObject, error) {
	img, err := s.ImageManager.GetImage(ctx, request.Id)
	if err != nil {
		return oapi.GetImage404JSONResponse{
			Code:    "not_found",
			Message: err.Error(),
		}, nil
	}
	return oapi.GetImage200JSONResponse(*img), nil
}

// DeleteImage deletes an image
func (s *ApiService) DeleteImage(ctx context.Context, request oapi.DeleteImageRequestObject) (oapi.DeleteImageResponseObject, error) {
	err := s.ImageManager.DeleteImage(ctx, request.Id)
	if err != nil {
		return oapi.DeleteImage404JSONResponse{
			Code:    "not_found",
			Message: err.Error(),
		}, nil
	}
	return oapi.DeleteImage204Response{}, nil
}

