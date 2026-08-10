package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"

	"github.com/kernel/hypeman/lib/builders"
	"github.com/kernel/hypeman/lib/builds"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/tags"
)

var (
	// maxBuildSourceSize bounds the source tarball accepted by POST /builds.
	// A var so tests can exercise the limit without large uploads.
	maxBuildSourceSize int64 = 512 << 20 // 512 MiB
	// maxBuildFormFieldSize bounds each small non-source multipart field
	// (dockerfile, secrets, tags, and friends).
	maxBuildFormFieldSize int64 = 1 << 20 // 1 MiB
)

// readLimitedPart reads a multipart part fully, erroring when its contents
// exceed limit bytes.
func readLimitedPart(part *multipart.Part, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(part, limit+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read %s field: %w", part.FormName(), err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds the maximum size of %d bytes", part.FormName(), limit)
	}
	return data, nil
}

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

	oapiBuilds := make([]oapi.Build, 0, len(domainBuilds))
	for _, b := range domainBuilds {
		if b == nil || !matchesTagsFilter(b.Tags, request.Params.Tags) {
			continue
		}
		oapiBuilds = append(oapiBuilds, buildToOAPI(b))
	}

	return oapi.ListBuilds200JSONResponse(oapiBuilds), nil
}

// CreateBuild creates a new build job
func (s *ApiService) CreateBuild(ctx context.Context, request oapi.CreateBuildRequestObject) (oapi.CreateBuildResponseObject, error) {
	log := logger.FromContext(ctx)

	// Parse multipart form fields
	var sourceData []byte
	var baseImageDigest, builderID, cacheScope, dockerfile, globalCacheKey, imageName string
	var timeoutSeconds, memoryMB, cpus int
	var isAdminBuild bool
	var secrets []builds.SecretRef
	var resourceTags map[string]string

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

		name := part.FormName()
		limit := maxBuildFormFieldSize
		if name == "source" {
			limit = maxBuildSourceSize
		}
		data, err := readLimitedPart(part, limit)
		part.Close()
		if err != nil {
			code := "invalid_request"
			if name == "source" {
				code = "invalid_source"
			}
			return oapi.CreateBuild400JSONResponse{
				Code:    code,
				Message: err.Error(),
			}, nil
		}

		switch name {
		case "source":
			sourceData = data
		case "base_image_digest":
			baseImageDigest = string(data)
		case "builder_id":
			builderID = string(data)
		case "cache_scope":
			cacheScope = string(data)
		case "dockerfile":
			dockerfile = string(data)
		case "timeout_seconds":
			if v, err := strconv.Atoi(string(data)); err == nil {
				timeoutSeconds = v
			}
		case "memory_mb":
			if v, err := strconv.Atoi(string(data)); err == nil {
				memoryMB = v
			}
		case "cpus":
			if v, err := strconv.Atoi(string(data)); err == nil {
				cpus = v
			}
		case "secrets":
			if err := json.Unmarshal(data, &secrets); err != nil {
				return oapi.CreateBuild400JSONResponse{
					Code:    "invalid_request",
					Message: "secrets must be a JSON array of {\"id\": \"...\", \"env_var\": \"...\"} objects",
				}, nil
			}
		case "is_admin_build":
			isAdminBuild = string(data) == "true" || string(data) == "1"
		case "global_cache_key":
			globalCacheKey = string(data)
		case "image_name":
			imageName = string(data)
		case "tags":
			parsed, err := parseTagsJSON(string(data))
			if err != nil {
				return oapi.CreateBuild400JSONResponse{
					Code:    "invalid_request",
					Message: "tags must be a JSON object with string key-value pairs",
				}, nil
			}
			resourceTags = parsed
		}
	}

	if len(sourceData) == 0 {
		return oapi.CreateBuild400JSONResponse{
			Code:    "invalid_request",
			Message: "source is required",
		}, nil
	}

	// Reject malformed secret IDs at the boundary so a bad reference fails
	// the request instead of the build.
	for _, secret := range secrets {
		if err := builds.ValidateSecretID(secret.ID); err != nil {
			return oapi.CreateBuild400JSONResponse{
				Code:    "invalid_request",
				Message: err.Error(),
			}, nil
		}
	}

	// Validate image_name early so the user gets a fast 400 instead of
	// a successful build that silently falls back to builds/{id}.
	if imageName != "" {
		if _, err := images.ParseNormalizedRef(imageName); err != nil {
			return oapi.CreateBuild400JSONResponse{
				Code:    "invalid_request",
				Message: fmt.Sprintf("invalid image_name: %v", err),
			}, nil
		}
	}

	// Note: Dockerfile validation happens in the builder agent.
	// It will check if Dockerfile is in the source tarball or provided via dockerfile parameter.

	// Build domain request
	domainReq := builds.CreateBuildRequest{
		BaseImageDigest: baseImageDigest,
		BuilderID:       builderID,
		CacheScope:      cacheScope,
		Dockerfile:      dockerfile,
		Secrets:         secrets,
		IsAdminBuild:    isAdminBuild,
		GlobalCacheKey:  globalCacheKey,
		ImageName:       imageName,
		Tags:            resourceTags,
	}

	// Apply build policy if any field was provided
	if timeoutSeconds > 0 || memoryMB > 0 || cpus > 0 {
		domainReq.BuildPolicy = &builds.BuildPolicy{
			TimeoutSeconds: timeoutSeconds,
			MemoryMB:       memoryMB,
			CPUs:           cpus,
		}
		if err := domainReq.BuildPolicy.Validate(); err != nil {
			return oapi.CreateBuild400JSONResponse{
				Code:    "invalid_request",
				Message: err.Error(),
			}, nil
		}
	}

	build, err := s.BuildManager.CreateBuild(ctx, domainReq, sourceData)
	if err != nil {
		switch {
		case errors.Is(err, tags.ErrInvalidTags):
			return oapi.CreateBuild400JSONResponse{
				Code:    "invalid_request",
				Message: err.Error(),
			}, nil
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
		case errors.Is(err, builders.ErrNotFound):
			return oapi.CreateBuild404JSONResponse{
				Code:    "not_found",
				Message: "builder not found",
			}, nil
		case errors.Is(err, builders.ErrInUse):
			return oapi.CreateBuild409JSONResponse{
				Code:    "conflict",
				Message: "builder is in use",
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
		Id:                b.ID,
		Status:            oapi.BuildStatus(b.Status),
		Tags:              toOAPITags(b.Tags),
		QueuePosition:     b.QueuePosition,
		ImageDigest:       b.ImageDigest,
		ImageRef:          b.ImageRef,
		Error:             b.Error,
		CreatedAt:         b.CreatedAt,
		StartedAt:         b.StartedAt,
		CompletedAt:       b.CompletedAt,
		DurationMs:        b.DurationMS,
		BuilderInstanceId: b.BuilderInstanceID,
		BuilderId:         b.BuilderID,
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
