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

// VolumeService contains methods and other services that help with interacting
// with the hypeman API.
//
// Note, unlike clients, this service does not read variables from the environment
// automatically. You should not instantiate this service directly, and instead use
// the [NewVolumeService] method instead.
type VolumeService struct {
	Options []option.RequestOption
}

// NewVolumeService generates a new service that applies the given options to each
// request. These options are applied after the parent client's options (if there
// is one), and before any request-specific options.
func NewVolumeService(opts ...option.RequestOption) (r VolumeService) {
	r = VolumeService{}
	r.Options = opts
	return
}

// Create volume
func (r *VolumeService) New(ctx context.Context, body VolumeNewParams, opts ...option.RequestOption) (res *Volume, err error) {
	opts = slices.Concat(r.Options, opts)
	path := "volumes"
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodPost, path, body, &res, opts...)
	return
}

// Get volume details
func (r *VolumeService) Get(ctx context.Context, id string, opts ...option.RequestOption) (res *Volume, err error) {
	opts = slices.Concat(r.Options, opts)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("volumes/%s", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodGet, path, nil, &res, opts...)
	return
}

// List volumes
func (r *VolumeService) List(ctx context.Context, opts ...option.RequestOption) (res *[]Volume, err error) {
	opts = slices.Concat(r.Options, opts)
	path := "volumes"
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodGet, path, nil, &res, opts...)
	return
}

// Delete volume
func (r *VolumeService) Delete(ctx context.Context, id string, opts ...option.RequestOption) (err error) {
	opts = slices.Concat(r.Options, opts)
	opts = append([]option.RequestOption{option.WithHeader("Accept", "")}, opts...)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("volumes/%s", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodDelete, path, nil, nil, opts...)
	return
}

type Volume struct {
	// Unique identifier
	ID string `json:"id,required"`
	// Creation timestamp (RFC3339)
	CreatedAt time.Time `json:"created_at,required" format:"date-time"`
	// Volume name
	Name string `json:"name,required"`
	// Size in gigabytes
	SizeGB int64 `json:"size_gb,required"`
	// Instance ID if attached
	AttachedTo string `json:"attached_to,nullable"`
	// Mount path if attached
	MountPath string `json:"mount_path,nullable"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		ID          respjson.Field
		CreatedAt   respjson.Field
		Name        respjson.Field
		SizeGB      respjson.Field
		AttachedTo  respjson.Field
		MountPath   respjson.Field
		ExtraFields map[string]respjson.Field
		raw         string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r Volume) RawJSON() string { return r.JSON.raw }
func (r *Volume) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type VolumeNewParams struct {
	// Volume name
	Name string `json:"name,required"`
	// Size in gigabytes
	SizeGB int64 `json:"size_gb,required"`
	// Optional custom identifier (auto-generated if not provided)
	ID param.Opt[string] `json:"id,omitzero"`
	paramObj
}

func (r VolumeNewParams) MarshalJSON() (data []byte, err error) {
	type shadow VolumeNewParams
	return param.MarshalObject(r, (*shadow)(&r))
}
func (r *VolumeNewParams) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}
