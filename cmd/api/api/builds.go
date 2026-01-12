package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"

	"github.com/onkernel/hypeman/lib/builds"
	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/oapi"
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
	var runtime string
	var baseImageDigest, cacheScope, dockerfile string
	var timeoutSeconds int

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
		case "runtime":
			var buf bytes.Buffer
			io.Copy(&buf, part)
			runtime = buf.String()
		case "base_image_digest":
			var buf bytes.Buffer
			io.Copy(&buf, part)
			baseImageDigest = buf.String()
		case "cache_scope":
			var buf bytes.Buffer
			io.Copy(&buf, part)
			cacheScope = buf.String()
		case "dockerfile":
			var buf bytes.Buffer
			io.Copy(&buf, part)
			dockerfile = buf.String()
		case "timeout_seconds":
			var buf bytes.Buffer
			io.Copy(&buf, part)
			if v, err := strconv.Atoi(buf.String()); err == nil {
				timeoutSeconds = v
			}
		}
		part.Close()
	}

	if runtime == "" {
		return oapi.CreateBuild400JSONResponse{
			Code:    "invalid_request",
			Message: "runtime is required",
		}, nil
	}

	if len(sourceData) == 0 {
		return oapi.CreateBuild400JSONResponse{
			Code:    "invalid_request",
			Message: "source is required",
		}, nil
	}

	// Build domain request
	domainReq := builds.CreateBuildRequest{
		Runtime:         runtime,
		BaseImageDigest: baseImageDigest,
		CacheScope:      cacheScope,
		Dockerfile:      dockerfile,
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
		case errors.Is(err, builds.ErrInvalidRuntime):
			return oapi.CreateBuild400JSONResponse{
				Code:    "invalid_runtime",
				Message: err.Error(),
			}, nil
		case errors.Is(err, builds.ErrInvalidSource):
			return oapi.CreateBuild400JSONResponse{
				Code:    "invalid_source",
				Message: err.Error(),
			}, nil
		case errors.Is(err, builds.ErrResourcesExhausted):
			retryAfter := 30 // Suggest retry after 30 seconds
			return oapi.CreateBuild503JSONResponse{
				Body: oapi.Error{
					Code:    "resources_exhausted",
					Message: "Insufficient resources to start build. Please retry later.",
				},
				Headers: oapi.CreateBuild503ResponseHeaders{
					RetryAfter: retryAfter,
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

// GetBuildLogs streams build logs
func (s *ApiService) GetBuildLogs(ctx context.Context, request oapi.GetBuildLogsRequestObject) (oapi.GetBuildLogsResponseObject, error) {
	log := logger.FromContext(ctx)

	logs, err := s.BuildManager.GetBuildLogs(ctx, request.Id)
	if err != nil {
		if errors.Is(err, builds.ErrNotFound) {
			return oapi.GetBuildLogs404JSONResponse{
				Code:    "not_found",
				Message: "build not found",
			}, nil
		}
		log.ErrorContext(ctx, "failed to get build logs", "error", err, "id", request.Id)
		return oapi.GetBuildLogs500JSONResponse{
			Code:    "internal_error",
			Message: "failed to get build logs",
		}, nil
	}

	// Return logs as SSE
	// For simplicity, return all logs at once
	// TODO: Implement proper SSE streaming with follow support
	return oapi.GetBuildLogs200TexteventStreamResponse{
		Body:          stringReader(string(logs)),
		ContentLength: int64(len(logs)),
	}, nil
}

// buildToOAPI converts a domain Build to OAPI Build
func buildToOAPI(b *builds.Build) oapi.Build {
	oapiBuild := oapi.Build{
		Id:            b.ID,
		Status:        oapi.BuildStatus(b.Status),
		Runtime:       b.Runtime,
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
			BaseImageDigest:  &b.Provenance.BaseImageDigest,
			SourceHash:       &b.Provenance.SourceHash,
			ToolchainVersion: &b.Provenance.ToolchainVersion,
			BuildkitVersion:  &b.Provenance.BuildkitVersion,
			Timestamp:        &b.Provenance.Timestamp,
		}
		if len(b.Provenance.LockfileHashes) > 0 {
			oapiBuild.Provenance.LockfileHashes = &b.Provenance.LockfileHashes
		}
	}

	return oapiBuild
}

// deref safely dereferences a pointer, returning empty string if nil
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// stringReader wraps a string as an io.Reader
type stringReaderImpl struct {
	s string
	i int
}

func stringReader(s string) io.Reader {
	return &stringReaderImpl{s: s}
}

func (r *stringReaderImpl) Read(p []byte) (n int, err error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n = copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}

