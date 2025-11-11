// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

package hypeman

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/onkernel/hypeman-go/internal/apijson"
	"github.com/onkernel/hypeman-go/internal/requestconfig"
	"github.com/onkernel/hypeman-go/option"
	"github.com/onkernel/hypeman-go/packages/param"
	"github.com/onkernel/hypeman-go/packages/respjson"
)

// ImageService contains methods and other services that help with interacting with
// the hypeman API.
//
// Note, unlike clients, this service does not read variables from the environment
// automatically. You should not instantiate this service directly, and instead use
// the [NewImageService] method instead.
type ImageService struct {
	Options []option.RequestOption
}

// NewImageService generates a new service that applies the given options to each
// request. These options are applied after the parent client's options (if there
// is one), and before any request-specific options.
func NewImageService(opts ...option.RequestOption) (r ImageService) {
	r = ImageService{}
	r.Options = opts
	return
}

// Pull and convert OCI image
func (r *ImageService) New(ctx context.Context, body ImageNewParams, opts ...option.RequestOption) (res *Image, err error) {
	opts = slices.Concat(r.Options, opts)
	path := "images"
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodPost, path, body, &res, opts...)
	return
}

// Get image details
func (r *ImageService) Get(ctx context.Context, name string, opts ...option.RequestOption) (res *Image, err error) {
	opts = slices.Concat(r.Options, opts)
	if name == "" {
		err = errors.New("missing required name parameter")
		return
	}
	path := fmt.Sprintf("images/%s", name)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodGet, path, nil, &res, opts...)
	return
}

// List images
func (r *ImageService) List(ctx context.Context, opts ...option.RequestOption) (res *[]Image, err error) {
	opts = slices.Concat(r.Options, opts)
	path := "images"
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodGet, path, nil, &res, opts...)
	return
}

// Delete image
func (r *ImageService) Delete(ctx context.Context, name string, opts ...option.RequestOption) (err error) {
	opts = slices.Concat(r.Options, opts)
	opts = append([]option.RequestOption{option.WithHeader("Accept", "")}, opts...)
	if name == "" {
		err = errors.New("missing required name parameter")
		return
	}
	path := fmt.Sprintf("images/%s", name)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodDelete, path, nil, nil, opts...)
	return
}

type Image struct {
	// Creation timestamp (RFC3339)
	CreatedAt time.Time `json:"created_at,required" format:"date-time"`
	// Resolved manifest digest
	Digest string `json:"digest,required"`
	// Normalized OCI image reference (tag or digest)
	Name string `json:"name,required"`
	// Build status
	//
	// Any of "pending", "pulling", "converting", "ready", "failed".
	Status ImageStatus `json:"status,required"`
	// CMD from container metadata
	Cmd []string `json:"cmd,nullable"`
	// Entrypoint from container metadata
	Entrypoint []string `json:"entrypoint,nullable"`
	// Environment variables from container metadata
	Env map[string]string `json:"env"`
	// Error message if status is failed
	Error string `json:"error,nullable"`
	// Position in build queue (null if not queued)
	QueuePosition int64 `json:"queue_position,nullable"`
	// Disk size in bytes (null until ready)
	SizeBytes int64 `json:"size_bytes,nullable"`
	// Working directory from container metadata
	WorkingDir string `json:"working_dir,nullable"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		CreatedAt     respjson.Field
		Digest        respjson.Field
		Name          respjson.Field
		Status        respjson.Field
		Cmd           respjson.Field
		Entrypoint    respjson.Field
		Env           respjson.Field
		Error         respjson.Field
		QueuePosition respjson.Field
		SizeBytes     respjson.Field
		WorkingDir    respjson.Field
		ExtraFields   map[string]respjson.Field
		raw           string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r Image) RawJSON() string { return r.JSON.raw }
func (r *Image) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

// Build status
type ImageStatus string

const (
	ImageStatusPending    ImageStatus = "pending"
	ImageStatusPulling    ImageStatus = "pulling"
	ImageStatusConverting ImageStatus = "converting"
	ImageStatusReady      ImageStatus = "ready"
	ImageStatusFailed     ImageStatus = "failed"
)

type ImageNewParams struct {
	// OCI image reference (e.g., docker.io/library/nginx:latest)
	Name string `json:"name,required"`
	paramObj
}

func (r ImageNewParams) MarshalJSON() (data []byte, err error) {
	type shadow ImageNewParams
	return param.MarshalObject(r, (*shadow)(&r))
}
func (r *ImageNewParams) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}
