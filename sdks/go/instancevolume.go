// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

package hypeman

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/stainless-sdks/hypeman-go/internal/apijson"
	"github.com/stainless-sdks/hypeman-go/internal/requestconfig"
	"github.com/stainless-sdks/hypeman-go/option"
	"github.com/stainless-sdks/hypeman-go/packages/param"
)

// InstanceVolumeService contains methods and other services that help with
// interacting with the hypeman API.
//
// Note, unlike clients, this service does not read variables from the environment
// automatically. You should not instantiate this service directly, and instead use
// the [NewInstanceVolumeService] method instead.
type InstanceVolumeService struct {
	Options []option.RequestOption
}

// NewInstanceVolumeService generates a new service that applies the given options
// to each request. These options are applied after the parent client's options (if
// there is one), and before any request-specific options.
func NewInstanceVolumeService(opts ...option.RequestOption) (r InstanceVolumeService) {
	r = InstanceVolumeService{}
	r.Options = opts
	return
}

// Attach volume to instance
func (r *InstanceVolumeService) Attach(ctx context.Context, volumeID string, params InstanceVolumeAttachParams, opts ...option.RequestOption) (res *Instance, err error) {
	opts = slices.Concat(r.Options, opts)
	if params.ID == "" {
		err = errors.New("missing required id parameter")
		return
	}
	if volumeID == "" {
		err = errors.New("missing required volumeId parameter")
		return
	}
	path := fmt.Sprintf("instances/%s/volumes/%s", params.ID, volumeID)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodPost, path, params, &res, opts...)
	return
}

// Detach volume from instance
func (r *InstanceVolumeService) Detach(ctx context.Context, volumeID string, body InstanceVolumeDetachParams, opts ...option.RequestOption) (res *Instance, err error) {
	opts = slices.Concat(r.Options, opts)
	if body.ID == "" {
		err = errors.New("missing required id parameter")
		return
	}
	if volumeID == "" {
		err = errors.New("missing required volumeId parameter")
		return
	}
	path := fmt.Sprintf("instances/%s/volumes/%s", body.ID, volumeID)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodDelete, path, nil, &res, opts...)
	return
}

type InstanceVolumeAttachParams struct {
	ID string `path:"id,required" json:"-"`
	// Path where volume should be mounted
	MountPath string `json:"mount_path,required"`
	// Mount as read-only
	Readonly param.Opt[bool] `json:"readonly,omitzero"`
	paramObj
}

func (r InstanceVolumeAttachParams) MarshalJSON() (data []byte, err error) {
	type shadow InstanceVolumeAttachParams
	return param.MarshalObject(r, (*shadow)(&r))
}
func (r *InstanceVolumeAttachParams) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type InstanceVolumeDetachParams struct {
	ID string `path:"id,required" json:"-"`
	paramObj
}
