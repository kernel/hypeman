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

// Creates a new empty volume of the specified size.
func (r *VolumeService) New(ctx context.Context, body VolumeNewParams, opts ...option.RequestOption) (res *Volume, err error) {
	opts = slices.Concat(r.Options, opts)
	path := "volumes"
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodPost, path, body, &res, opts...)
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
	opts = append([]option.RequestOption{option.WithHeader("Accept", "*/*")}, opts...)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("volumes/%s", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodDelete, path, nil, nil, opts...)
	return
}

// Creates a new volume pre-populated with content from a tar.gz archive. The
// archive is streamed directly into the volume's root directory.
func (r *VolumeService) NewFromArchive(ctx context.Context, body io.Reader, params VolumeNewFromArchiveParams, opts ...option.RequestOption) (res *Volume, err error) {
	opts = slices.Concat(r.Options, opts)
	opts = append([]option.RequestOption{option.WithRequestBody("application/gzip", body)}, opts...)
	path := "volumes/from-archive"
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodPost, path, nil, &res, opts...)
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

type Volume struct {
	// Unique identifier
	ID string `json:"id,required"`
	// Creation timestamp (RFC3339)
	CreatedAt time.Time `json:"created_at,required" format:"date-time"`
	// Volume name
	Name string `json:"name,required"`
	// Size in gigabytes
	SizeGB int64 `json:"size_gb,required"`
	// List of current attachments (empty if not attached)
	Attachments []VolumeAttachment `json:"attachments"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		ID          respjson.Field
		CreatedAt   respjson.Field
		Name        respjson.Field
		SizeGB      respjson.Field
		Attachments respjson.Field
		ExtraFields map[string]respjson.Field
		raw         string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r Volume) RawJSON() string { return r.JSON.raw }
func (r *Volume) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type VolumeAttachment struct {
	// ID of the instance this volume is attached to
	InstanceID string `json:"instance_id,required"`
	// Mount path in the guest
	MountPath string `json:"mount_path,required"`
	// Whether the attachment is read-only
	Readonly bool `json:"readonly,required"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		InstanceID  respjson.Field
		MountPath   respjson.Field
		Readonly    respjson.Field
		ExtraFields map[string]respjson.Field
		raw         string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r VolumeAttachment) RawJSON() string { return r.JSON.raw }
func (r *VolumeAttachment) UnmarshalJSON(data []byte) error {
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

type VolumeNewFromArchiveParams struct {
	// Volume name
	Name string `query:"name,required" json:"-"`
	// Maximum size in GB (extraction fails if content exceeds this)
	SizeGB int64 `query:"size_gb,required" json:"-"`
	// Optional custom volume ID (auto-generated if not provided)
	ID param.Opt[string] `query:"id,omitzero" json:"-"`
	paramObj
}

func (r VolumeNewFromArchiveParams) MarshalMultipart() (data []byte, contentType string, err error) {
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

// URLQuery serializes [VolumeNewFromArchiveParams]'s query parameters as
// `url.Values`.
func (r VolumeNewFromArchiveParams) URLQuery() (v url.Values, err error) {
	return apiquery.MarshalWithSettings(r, apiquery.QuerySettings{
		ArrayFormat:  apiquery.ArrayQueryFormatComma,
		NestedFormat: apiquery.NestedQueryFormatBrackets,
	})
}
