package api

import (
	"context"
	"errors"

	"github.com/kernel/hypeman/lib/builders"
	"github.com/kernel/hypeman/lib/logger"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/tags"
)

// ListBuilders lists all builders
func (s *ApiService) ListBuilders(ctx context.Context, request oapi.ListBuildersRequestObject) (oapi.ListBuildersResponseObject, error) {
	log := logger.FromContext(ctx)

	domainBuilders, err := s.BuilderManager.ListBuilders(ctx)
	if err != nil {
		log.ErrorContext(ctx, "failed to list builders", "error", err)
		return oapi.ListBuilders500JSONResponse{
			Code:    "internal_error",
			Message: "failed to list builders",
		}, nil
	}

	oapiBuilders := make([]oapi.Builder, 0, len(domainBuilders))
	for _, b := range domainBuilders {
		if !matchesTagsFilter(b.Tags, request.Params.Tags) {
			continue
		}
		oapiBuilders = append(oapiBuilders, s.builderToOAPI(&b))
	}

	return oapi.ListBuilders200JSONResponse(oapiBuilders), nil
}

// CreateBuilder creates a new builder
func (s *ApiService) CreateBuilder(ctx context.Context, request oapi.CreateBuilderRequestObject) (oapi.CreateBuilderResponseObject, error) {
	log := logger.FromContext(ctx)

	if request.Body == nil {
		return oapi.CreateBuilder400JSONResponse{
			Code:    "invalid_request",
			Message: "request body is required",
		}, nil
	}

	if request.Body.DiskSizeGb != nil && *request.Body.DiskSizeGb <= 0 {
		return oapi.CreateBuilder400JSONResponse{
			Code:    "invalid_request",
			Message: "disk_size_gb must be positive",
		}, nil
	}

	diskSizeGb := 0
	if request.Body.DiskSizeGb != nil {
		diskSizeGb = *request.Body.DiskSizeGb
	}
	domainReq := builders.CreateBuilderRequest{
		ID:         request.Body.Id,
		Name:       derefString(request.Body.Name),
		DiskSizeGb: diskSizeGb,
		Tags:       toMapTags(request.Body.Tags),
	}

	b, err := s.BuilderManager.CreateBuilder(ctx, domainReq)
	if err != nil {
		switch {
		case errors.Is(err, builders.ErrAlreadyExists):
			return oapi.CreateBuilder409JSONResponse{
				Code:    "already_exists",
				Message: "builder with this ID already exists",
			}, nil
		case errors.Is(err, builders.ErrQuotaExceeded):
			return oapi.CreateBuilder409JSONResponse{
				Code:    "quota_exceeded",
				Message: err.Error(),
			}, nil
		case errors.Is(err, builders.ErrInvalidID), errors.Is(err, builders.ErrInvalidDiskSize):
			return oapi.CreateBuilder400JSONResponse{
				Code:    "invalid_request",
				Message: err.Error(),
			}, nil
		case errors.Is(err, builders.ErrDiskSizeExceeded):
			return oapi.CreateBuilder400JSONResponse{
				Code:    "invalid_request",
				Message: err.Error(),
			}, nil
		case errors.Is(err, tags.ErrInvalidTags):
			return oapi.CreateBuilder400JSONResponse{
				Code:    "invalid_request",
				Message: err.Error(),
			}, nil
		}
		log.ErrorContext(ctx, "failed to create builder", "error", err)
		return oapi.CreateBuilder500JSONResponse{
			Code:    "internal_error",
			Message: "failed to create builder",
		}, nil
	}

	return oapi.CreateBuilder201JSONResponse(s.builderToOAPI(b)), nil
}

// GetBuilder gets builder details
// Note: Resolution is handled by ResolveResource middleware
func (s *ApiService) GetBuilder(ctx context.Context, request oapi.GetBuilderRequestObject) (oapi.GetBuilderResponseObject, error) {
	b := mw.GetResolvedBuilder[builders.Builder](ctx)
	if b == nil {
		return oapi.GetBuilder500JSONResponse{
			Code:    "internal_error",
			Message: "resource not resolved",
		}, nil
	}
	return oapi.GetBuilder200JSONResponse(s.builderToOAPI(b)), nil
}

// DeleteBuilder deletes a builder and its cache disk
// Note: Resolution is handled by ResolveResource middleware
func (s *ApiService) DeleteBuilder(ctx context.Context, request oapi.DeleteBuilderRequestObject) (oapi.DeleteBuilderResponseObject, error) {
	b := mw.GetResolvedBuilder[builders.Builder](ctx)
	if b == nil {
		return oapi.DeleteBuilder500JSONResponse{
			Code:    "internal_error",
			Message: "resource not resolved",
		}, nil
	}
	log := logger.FromContext(ctx)

	err := s.BuilderManager.DeleteBuilder(ctx, b.ID)
	if err != nil {
		if errors.Is(err, builders.ErrNotFound) {
			// Deleted between resolution and this call (e.g. idle reaper)
			return oapi.DeleteBuilder404JSONResponse{
				Code:    "not_found",
				Message: "builder not found",
			}, nil
		}
		if errors.Is(err, builders.ErrInUse) {
			return oapi.DeleteBuilder409JSONResponse{
				Code:    "conflict",
				Message: "builder is in use",
			}, nil
		}
		log.ErrorContext(ctx, "failed to delete builder", "error", err)
		return oapi.DeleteBuilder500JSONResponse{
			Code:    "internal_error",
			Message: "failed to delete builder",
		}, nil
	}
	return oapi.DeleteBuilder204Response{}, nil
}

// PruneBuilder resets a builder's cache by recreating its disk
// Note: Resolution is handled by ResolveResource middleware
func (s *ApiService) PruneBuilder(ctx context.Context, request oapi.PruneBuilderRequestObject) (oapi.PruneBuilderResponseObject, error) {
	b := mw.GetResolvedBuilder[builders.Builder](ctx)
	if b == nil {
		return oapi.PruneBuilder500JSONResponse{
			Code:    "internal_error",
			Message: "resource not resolved",
		}, nil
	}
	log := logger.FromContext(ctx)

	accepted, err := s.BuilderManager.ResetDisk(ctx, b.ID)
	if err != nil {
		if errors.Is(err, builders.ErrNotFound) {
			// Deleted between resolution and this call (e.g. idle reaper)
			return oapi.PruneBuilder404JSONResponse{
				Code:    "not_found",
				Message: "builder not found",
			}, nil
		}
		if errors.Is(err, builders.ErrInUse) {
			return oapi.PruneBuilder409JSONResponse{
				Code:    "conflict",
				Message: "builder is in use",
			}, nil
		}
		log.ErrorContext(ctx, "failed to prune builder", "error", err)
		return oapi.PruneBuilder500JSONResponse{
			Code:    "internal_error",
			Message: "failed to prune builder",
		}, nil
	}

	return oapi.PruneBuilder202JSONResponse(s.builderToOAPI(accepted)), nil
}

// builderToOAPI converts a domain builder to its API representation. One
// build at a time runs on a builder, so max_concurrency is fixed at 1;
// active_build_id and queued_builds come from the build queue.
func (s *ApiService) builderToOAPI(b *builders.Builder) oapi.Builder {
	return oapi.Builder{
		Id:             b.ID,
		Name:           stringPtrOrNil(b.Name),
		DiskSizeGb:     b.DiskSizeGb,
		Status:         oapi.BuilderStatus(b.Status),
		Tags:           toOAPITags(b.Tags),
		CreatedAt:      b.CreatedAt,
		LastUsedAt:     b.LastUsedAt,
		MaxConcurrency: 1,
		ActiveBuildId:  s.BuildManager.ActiveBuildForBuilder(b.ID),
		QueuedBuilds:   s.BuildManager.QueuedBuildsForBuilder(b.ID),
	}
}

func stringPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
