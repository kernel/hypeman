// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

package hypeman

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/kernel/hypeman-go/internal/apiform"
	"github.com/kernel/hypeman-go/internal/apijson"
	"github.com/kernel/hypeman-go/internal/apiquery"
	"github.com/kernel/hypeman-go/internal/requestconfig"
	"github.com/kernel/hypeman-go/option"
	"github.com/kernel/hypeman-go/packages/param"
	"github.com/kernel/hypeman-go/packages/respjson"
	"github.com/kernel/hypeman-go/packages/ssestream"
)

// BuildService contains methods and other services that help with interacting with
// the hypeman API.
//
// Note, unlike clients, this service does not read variables from the environment
// automatically. You should not instantiate this service directly, and instead use
// the [NewBuildService] method instead.
type BuildService struct {
	Options []option.RequestOption
}

// NewBuildService generates a new service that applies the given options to each
// request. These options are applied after the parent client's options (if there
// is one), and before any request-specific options.
func NewBuildService(opts ...option.RequestOption) (r BuildService) {
	r = BuildService{}
	r.Options = opts
	return
}

// Creates a new build job. Source code should be uploaded as a tar.gz archive in
// the multipart form data.
func (r *BuildService) New(ctx context.Context, body BuildNewParams, opts ...option.RequestOption) (res *Build, err error) {
	opts = slices.Concat(r.Options, opts)
	path := "builds"
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodPost, path, body, &res, opts...)
	return
}

// List builds
func (r *BuildService) List(ctx context.Context, opts ...option.RequestOption) (res *[]Build, err error) {
	opts = slices.Concat(r.Options, opts)
	path := "builds"
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodGet, path, nil, &res, opts...)
	return
}

// Cancel build
func (r *BuildService) Cancel(ctx context.Context, id string, opts ...option.RequestOption) (err error) {
	opts = slices.Concat(r.Options, opts)
	opts = append([]option.RequestOption{option.WithHeader("Accept", "*/*")}, opts...)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("builds/%s", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodDelete, path, nil, nil, opts...)
	return
}

// Streams build events as Server-Sent Events. Events include:
//
// - `log`: Build log lines with timestamp and content
// - `status`: Build status changes (queued→building→pushing→ready/failed)
// - `heartbeat`: Keep-alive events sent every 30s to prevent connection timeouts
//
// Returns existing logs as events, then continues streaming if follow=true.
func (r *BuildService) EventsStreaming(ctx context.Context, id string, query BuildEventsParams, opts ...option.RequestOption) (stream *ssestream.Stream[BuildEvent]) {
	var (
		raw *http.Response
		err error
	)
	opts = slices.Concat(r.Options, opts)
	opts = append([]option.RequestOption{option.WithHeader("Accept", "text/event-stream")}, opts...)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("builds/%s/events", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodGet, path, query, &raw, opts...)
	return ssestream.NewStream[BuildEvent](ssestream.NewDecoder(raw), err)
}

// Get build details
func (r *BuildService) Get(ctx context.Context, id string, opts ...option.RequestOption) (res *Build, err error) {
	opts = slices.Concat(r.Options, opts)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("builds/%s", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodGet, path, nil, &res, opts...)
	return
}

type Build struct {
	// Build job identifier
	ID string `json:"id,required"`
	// Build creation timestamp
	CreatedAt time.Time `json:"created_at,required" format:"date-time"`
	// Build job status
	//
	// Any of "queued", "building", "pushing", "ready", "failed", "cancelled".
	Status BuildStatus `json:"status,required"`
	// Instance ID of the builder VM (for debugging)
	BuilderInstanceID string `json:"builder_instance_id,nullable"`
	// Build completion timestamp
	CompletedAt time.Time `json:"completed_at,nullable" format:"date-time"`
	// Build duration in milliseconds
	DurationMs int64 `json:"duration_ms,nullable"`
	// Error message (only when status is failed)
	Error string `json:"error,nullable"`
	// Digest of built image (only when status is ready)
	ImageDigest string `json:"image_digest,nullable"`
	// Full image reference (only when status is ready)
	ImageRef   string          `json:"image_ref,nullable"`
	Provenance BuildProvenance `json:"provenance"`
	// Position in build queue (only when status is queued)
	QueuePosition int64 `json:"queue_position,nullable"`
	// Build start timestamp
	StartedAt time.Time `json:"started_at,nullable" format:"date-time"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		ID                respjson.Field
		CreatedAt         respjson.Field
		Status            respjson.Field
		BuilderInstanceID respjson.Field
		CompletedAt       respjson.Field
		DurationMs        respjson.Field
		Error             respjson.Field
		ImageDigest       respjson.Field
		ImageRef          respjson.Field
		Provenance        respjson.Field
		QueuePosition     respjson.Field
		StartedAt         respjson.Field
		ExtraFields       map[string]respjson.Field
		raw               string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r Build) RawJSON() string { return r.JSON.raw }
func (r *Build) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type BuildEvent struct {
	// Event timestamp
	Timestamp time.Time `json:"timestamp,required" format:"date-time"`
	// Event type
	//
	// Any of "log", "status", "heartbeat".
	Type BuildEventType `json:"type,required"`
	// Log line content (only for type=log)
	Content string `json:"content"`
	// New build status (only for type=status)
	//
	// Any of "queued", "building", "pushing", "ready", "failed", "cancelled".
	Status BuildStatus `json:"status"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		Timestamp   respjson.Field
		Type        respjson.Field
		Content     respjson.Field
		Status      respjson.Field
		ExtraFields map[string]respjson.Field
		raw         string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r BuildEvent) RawJSON() string { return r.JSON.raw }
func (r *BuildEvent) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

// Event type
type BuildEventType string

const (
	BuildEventTypeLog       BuildEventType = "log"
	BuildEventTypeStatus    BuildEventType = "status"
	BuildEventTypeHeartbeat BuildEventType = "heartbeat"
)

type BuildProvenance struct {
	// Pinned base image digest used
	BaseImageDigest string `json:"base_image_digest"`
	// BuildKit version used
	BuildkitVersion string `json:"buildkit_version"`
	// Map of lockfile names to SHA256 hashes
	LockfileHashes map[string]string `json:"lockfile_hashes"`
	// SHA256 hash of source tarball
	SourceHash string `json:"source_hash"`
	// Build completion timestamp
	Timestamp time.Time `json:"timestamp" format:"date-time"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		BaseImageDigest respjson.Field
		BuildkitVersion respjson.Field
		LockfileHashes  respjson.Field
		SourceHash      respjson.Field
		Timestamp       respjson.Field
		ExtraFields     map[string]respjson.Field
		raw             string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r BuildProvenance) RawJSON() string { return r.JSON.raw }
func (r *BuildProvenance) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

// Build job status
type BuildStatus string

const (
	BuildStatusQueued    BuildStatus = "queued"
	BuildStatusBuilding  BuildStatus = "building"
	BuildStatusPushing   BuildStatus = "pushing"
	BuildStatusReady     BuildStatus = "ready"
	BuildStatusFailed    BuildStatus = "failed"
	BuildStatusCancelled BuildStatus = "cancelled"
)

type BuildNewParams struct {
	// Source tarball (tar.gz) containing application code and optionally a Dockerfile
	Source io.Reader `json:"source,omitzero,required" format:"binary"`
	// Optional pinned base image digest
	BaseImageDigest param.Opt[string] `json:"base_image_digest,omitzero"`
	// Tenant-specific cache key prefix
	CacheScope param.Opt[string] `json:"cache_scope,omitzero"`
	// Dockerfile content. Required if not included in the source tarball.
	Dockerfile param.Opt[string] `json:"dockerfile,omitzero"`
	// Global cache identifier (e.g., "node", "python", "ubuntu", "browser"). When
	// specified, the build will import from cache/global/{key}. Admin builds will also
	// export to this location.
	GlobalCacheKey param.Opt[string] `json:"global_cache_key,omitzero"`
	// Set to "true" to grant push access to global cache (operator-only). Admin builds
	// can populate the shared global cache that all tenant builds read from.
	IsAdminBuild param.Opt[string] `json:"is_admin_build,omitzero"`
	// JSON array of secret references to inject during build. Each object has "id"
	// (required) for use with --mount=type=secret,id=... Example: [{"id":
	// "npm_token"}, {"id": "github_token"}]
	Secrets param.Opt[string] `json:"secrets,omitzero"`
	// Build timeout (default 600)
	TimeoutSeconds param.Opt[int64] `json:"timeout_seconds,omitzero"`
	paramObj
}

func (r BuildNewParams) MarshalMultipart() (data []byte, contentType string, err error) {
	buf := bytes.NewBuffer(nil)
	writer := multipart.NewWriter(buf)
	err = apiform.MarshalRoot(r, writer)
	if err == nil {
		err = apiform.WriteExtras(writer, r.ExtraFields())
	}
	if err != nil {
		writer.Close()
		return nil, "", err
	}
	err = writer.Close()
	if err != nil {
		return nil, "", err
	}
	return buf.Bytes(), writer.FormDataContentType(), nil
}

type BuildEventsParams struct {
	// Continue streaming new events after initial output
	Follow param.Opt[bool] `query:"follow,omitzero" json:"-"`
	paramObj
}

// URLQuery serializes [BuildEventsParams]'s query parameters as `url.Values`.
func (r BuildEventsParams) URLQuery() (v url.Values, err error) {
	return apiquery.MarshalWithSettings(r, apiquery.QuerySettings{
		ArrayFormat:  apiquery.ArrayQueryFormatComma,
		NestedFormat: apiquery.NestedQueryFormatBrackets,
	})
}
