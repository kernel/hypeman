package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/kernel/hypeman/lib/builds"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/oapi"
)

// ListBuilds returns all builds
func (s *ApiService) ListBuilds(ctx context.Context, request oapi.ListBuildsRequestObject) (oapi.ListBuildsResponseObject, error) {
	log := logger.FromContext(ctx)

	domainBuilds, err := s.BuildManager.ListBuilds(ctx)
	if err != nil {
		log.ErrorContext(ctx, "failed to list builds", "error", err)
		return oapi.ListBuilds500JSONResponse{
			Code:    "internal_error",
			Message: "failed to list builds",
		}, nil
	}

	oapiBuilds := make([]oapi.Build, len(domainBuilds))
	for i, b := range domainBuilds {
		oapiBuilds[i] = buildToOAPI(b)
	}

	return oapi.ListBuilds200JSONResponse(oapiBuilds), nil
}

// CreateBuild creates a new build job
func (s *ApiService) CreateBuild(ctx context.Context, request oapi.CreateBuildRequestObject) (oapi.CreateBuildResponseObject, error) {
	log := logger.FromContext(ctx)

	// Parse multipart form fields
	var sourceData []byte
	var baseImageDigest, cacheScope, dockerfile string
	var timeoutSeconds int
	var secrets []builds.SecretRef

	for {
		part, err := request.Body.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return oapi.CreateBuild400JSONResponse{
				Code:    "invalid_request",
				Message: "failed to parse multipart form",
			}, nil
		}

		switch part.FormName() {
		case "source":
			sourceData, err = io.ReadAll(part)
			if err != nil {
				return oapi.CreateBuild400JSONResponse{
					Code:    "invalid_source",
					Message: "failed to read source data",
				}, nil
			}
		case "base_image_digest":
			data, err := io.ReadAll(part)
			if err != nil {
				return oapi.CreateBuild400JSONResponse{
					Code:    "invalid_request",
					Message: "failed to read base_image_digest field",
				}, nil
			}
			baseImageDigest = string(data)
		case "cache_scope":
			data, err := io.ReadAll(part)
			if err != nil {
				return oapi.CreateBuild400JSONResponse{
					Code:    "invalid_request",
					Message: "failed to read cache_scope field",
				}, nil
			}
			cacheScope = string(data)
		case "dockerfile":
			data, err := io.ReadAll(part)
			if err != nil {
				return oapi.CreateBuild400JSONResponse{
					Code:    "invalid_request",
					Message: "failed to read dockerfile field",
				}, nil
			}
			dockerfile = string(data)
		case "timeout_seconds":
			data, err := io.ReadAll(part)
			if err != nil {
				return oapi.CreateBuild400JSONResponse{
					Code:    "invalid_request",
					Message: "failed to read timeout_seconds field",
				}, nil
			}
			if v, err := strconv.Atoi(string(data)); err == nil {
				timeoutSeconds = v
			}
		case "secrets":
			data, err := io.ReadAll(part)
			if err != nil {
				return oapi.CreateBuild400JSONResponse{
					Code:    "invalid_request",
					Message: "failed to read secrets field",
				}, nil
			}
			if err := json.Unmarshal(data, &secrets); err != nil {
				return oapi.CreateBuild400JSONResponse{
					Code:    "invalid_request",
					Message: "secrets must be a JSON array of {\"id\": \"...\", \"env_var\": \"...\"} objects",
				}, nil
			}
		}
		part.Close()
	}

	if len(sourceData) == 0 {
		return oapi.CreateBuild400JSONResponse{
			Code:    "invalid_request",
			Message: "source is required",
		}, nil
	}

	// Note: Dockerfile validation happens in the builder agent.
	// It will check if Dockerfile is in the source tarball or provided via dockerfile parameter.

	// Build domain request
	domainReq := builds.CreateBuildRequest{
		BaseImageDigest: baseImageDigest,
		CacheScope:      cacheScope,
		Dockerfile:      dockerfile,
		Secrets:         secrets,
	}

	// Apply timeout if provided
	if timeoutSeconds > 0 {
		domainReq.BuildPolicy = &builds.BuildPolicy{
			TimeoutSeconds: timeoutSeconds,
		}
	}

	build, err := s.BuildManager.CreateBuild(ctx, domainReq, sourceData)
	if err != nil {
		switch {
		case errors.Is(err, builds.ErrDockerfileRequired):
			return oapi.CreateBuild400JSONResponse{
				Code:    "dockerfile_required",
				Message: err.Error(),
			}, nil
		case errors.Is(err, builds.ErrInvalidSource):
			return oapi.CreateBuild400JSONResponse{
				Code:    "invalid_source",
				Message: err.Error(),
			}, nil
		case errors.Is(err, builds.ErrResourcesExhausted):
			return oapi.CreateBuild503JSONResponse{
				Body: oapi.Error{
					Code:    "resources_exhausted",
					Message: "insufficient resources for build, please retry later",
				},
				Headers: oapi.CreateBuild503ResponseHeaders{
					RetryAfter: 30,
				},
			}, nil
		default:
			log.ErrorContext(ctx, "failed to create build", "error", err)
			return oapi.CreateBuild500JSONResponse{
				Code:    "internal_error",
				Message: "failed to create build",
			}, nil
		}
	}

	return oapi.CreateBuild202JSONResponse(buildToOAPI(build)), nil
}

// GetBuild gets build details
func (s *ApiService) GetBuild(ctx context.Context, request oapi.GetBuildRequestObject) (oapi.GetBuildResponseObject, error) {
	log := logger.FromContext(ctx)

	build, err := s.BuildManager.GetBuild(ctx, request.Id)
	if err != nil {
		if errors.Is(err, builds.ErrNotFound) {
			return oapi.GetBuild404JSONResponse{
				Code:    "not_found",
				Message: "build not found",
			}, nil
		}
		log.ErrorContext(ctx, "failed to get build", "error", err, "id", request.Id)
		return oapi.GetBuild500JSONResponse{
			Code:    "internal_error",
			Message: "failed to get build",
		}, nil
	}

	return oapi.GetBuild200JSONResponse(buildToOAPI(build)), nil
}

// CancelBuild cancels a build
func (s *ApiService) CancelBuild(ctx context.Context, request oapi.CancelBuildRequestObject) (oapi.CancelBuildResponseObject, error) {
	log := logger.FromContext(ctx)

	err := s.BuildManager.CancelBuild(ctx, request.Id)
	if err != nil {
		switch {
		case errors.Is(err, builds.ErrNotFound):
			return oapi.CancelBuild404JSONResponse{
				Code:    "not_found",
				Message: "build not found",
			}, nil
		case errors.Is(err, builds.ErrBuildInProgress):
			return oapi.CancelBuild409JSONResponse{
				Code:    "conflict",
				Message: "build already in progress",
			}, nil
		default:
			log.ErrorContext(ctx, "failed to cancel build", "error", err, "id", request.Id)
			return oapi.CancelBuild500JSONResponse{
				Code:    "internal_error",
				Message: "failed to cancel build",
			}, nil
		}
	}

	return oapi.CancelBuild204Response{}, nil
}

// GetBuildEvents streams build events via SSE
// With follow=false (default), streams existing logs then closes
// With follow=true, continues streaming until build completes
func (s *ApiService) GetBuildEvents(ctx context.Context, request oapi.GetBuildEventsRequestObject) (oapi.GetBuildEventsResponseObject, error) {
	log := logger.FromContext(ctx)

	// Parse follow parameter (default false)
	follow := false
	if request.Params.Follow != nil {
		follow = *request.Params.Follow
	}

	eventChan, err := s.BuildManager.StreamBuildEvents(ctx, request.Id, follow)
	if err != nil {
		if errors.Is(err, builds.ErrNotFound) {
			return oapi.GetBuildEvents404JSONResponse{
				Code:    "not_found",
				Message: "build not found",
			}, nil
		}
		log.ErrorContext(ctx, "failed to stream build events", "error", err, "id", request.Id)
		return oapi.GetBuildEvents500JSONResponse{
			Code:    "internal_error",
			Message: "failed to stream build events",
		}, nil
	}

	return buildEventsStreamResponse{eventChan: eventChan}, nil
}

// buildEventsStreamResponse implements oapi.GetBuildEventsResponseObject with proper SSE streaming
type buildEventsStreamResponse struct {
	eventChan <-chan builds.BuildEvent
}

func (r buildEventsStreamResponse) VisitGetBuildEventsResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering
	w.WriteHeader(200)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}

	for event := range r.eventChan {
		jsonEvent, err := json.Marshal(event)
		if err != nil {
			continue
		}
		fmt.Fprintf(w, "data: %s\n\n", jsonEvent)
		flusher.Flush()
	}
	return nil
}

// buildToOAPI converts a domain Build to OAPI Build
func buildToOAPI(b *builds.Build) oapi.Build {
	oapiBuild := oapi.Build{
		Id:            b.ID,
		Status:        oapi.BuildStatus(b.Status),
		QueuePosition: b.QueuePosition,
		ImageDigest:   b.ImageDigest,
		ImageRef:      b.ImageRef,
		Error:         b.Error,
		CreatedAt:     b.CreatedAt,
		StartedAt:     b.StartedAt,
		CompletedAt:   b.CompletedAt,
		DurationMs:    b.DurationMS,
	}

	if b.Provenance != nil {
		oapiBuild.Provenance = &oapi.BuildProvenance{
			BaseImageDigest: &b.Provenance.BaseImageDigest,
			SourceHash:      &b.Provenance.SourceHash,
			BuildkitVersion: &b.Provenance.BuildkitVersion,
			Timestamp:       &b.Provenance.Timestamp,
		}
		if len(b.Provenance.LockfileHashes) > 0 {
			oapiBuild.Provenance.LockfileHashes = &b.Provenance.LockfileHashes
		}
	}

	return oapiBuild
}

