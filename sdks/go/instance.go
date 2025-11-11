// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

package hypeman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/stainless-sdks/hypeman-go/internal/apijson"
	"github.com/stainless-sdks/hypeman-go/internal/apiquery"
	"github.com/stainless-sdks/hypeman-go/internal/requestconfig"
	"github.com/stainless-sdks/hypeman-go/option"
	"github.com/stainless-sdks/hypeman-go/packages/param"
	"github.com/stainless-sdks/hypeman-go/packages/respjson"
	"github.com/stainless-sdks/hypeman-go/packages/ssestream"
)

// InstanceService contains methods and other services that help with interacting
// with the hypeman API.
//
// Note, unlike clients, this service does not read variables from the environment
// automatically. You should not instantiate this service directly, and instead use
// the [NewInstanceService] method instead.
type InstanceService struct {
	Options []option.RequestOption
	Volumes InstanceVolumeService
}

// NewInstanceService generates a new service that applies the given options to
// each request. These options are applied after the parent client's options (if
// there is one), and before any request-specific options.
func NewInstanceService(opts ...option.RequestOption) (r InstanceService) {
	r = InstanceService{}
	r.Options = opts
	r.Volumes = NewInstanceVolumeService(opts...)
	return
}

// Create and start instance
func (r *InstanceService) New(ctx context.Context, body InstanceNewParams, opts ...option.RequestOption) (res *Instance, err error) {
	opts = slices.Concat(r.Options, opts)
	path := "instances"
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodPost, path, body, &res, opts...)
	return
}

// Get instance details
func (r *InstanceService) Get(ctx context.Context, id string, opts ...option.RequestOption) (res *Instance, err error) {
	opts = slices.Concat(r.Options, opts)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("instances/%s", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodGet, path, nil, &res, opts...)
	return
}

// List instances
func (r *InstanceService) List(ctx context.Context, opts ...option.RequestOption) (res *[]Instance, err error) {
	opts = slices.Concat(r.Options, opts)
	path := "instances"
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodGet, path, nil, &res, opts...)
	return
}

// Stop and delete instance
func (r *InstanceService) Delete(ctx context.Context, id string, opts ...option.RequestOption) (err error) {
	opts = slices.Concat(r.Options, opts)
	opts = append([]option.RequestOption{option.WithHeader("Accept", "")}, opts...)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("instances/%s", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodDelete, path, nil, nil, opts...)
	return
}

// Put instance in standby (pause, snapshot, delete VMM)
func (r *InstanceService) PutInStandby(ctx context.Context, id string, opts ...option.RequestOption) (res *Instance, err error) {
	opts = slices.Concat(r.Options, opts)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("instances/%s/standby", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodPost, path, nil, &res, opts...)
	return
}

// Restore instance from standby
func (r *InstanceService) RestoreFromStandby(ctx context.Context, id string, opts ...option.RequestOption) (res *Instance, err error) {
	opts = slices.Concat(r.Options, opts)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("instances/%s/restore", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodPost, path, nil, &res, opts...)
	return
}

// Stream instance logs (SSE)
func (r *InstanceService) StreamLogsStreaming(ctx context.Context, id string, query InstanceStreamLogsParams, opts ...option.RequestOption) (stream *ssestream.Stream[string]) {
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
	path := fmt.Sprintf("instances/%s/logs", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodGet, path, query, &raw, opts...)
	return ssestream.NewStream[string](ssestream.NewDecoder(raw), err)
}

type Instance struct {
	// Unique identifier
	ID string `json:"id,required"`
	// Creation timestamp (RFC3339)
	CreatedAt time.Time `json:"created_at,required" format:"date-time"`
	// Image identifier
	Image string `json:"image,required"`
	// Human-readable name
	Name string `json:"name,required"`
	// Instance state:
	//
	// - Created: VMM created but not started (Cloud Hypervisor native)
	// - Running: VM is actively running (Cloud Hypervisor native)
	// - Paused: VM is paused (Cloud Hypervisor native)
	// - Shutdown: VM shut down but VMM exists (Cloud Hypervisor native)
	// - Stopped: No VMM running, no snapshot exists
	// - Standby: No VMM running, snapshot exists (can be restored)
	//
	// Any of "Created", "Running", "Paused", "Shutdown", "Stopped", "Standby".
	State InstanceState `json:"state,required"`
	// Environment variables
	Env map[string]string `json:"env"`
	// Fully qualified domain name
	Fqdn string `json:"fqdn,nullable"`
	// Whether a snapshot exists for this instance
	HasSnapshot bool `json:"has_snapshot"`
	// Configured maximum memory in MB
	MemoryMaxMB int64 `json:"memory_max_mb"`
	// Configured base memory in MB
	MemoryMB int64 `json:"memory_mb"`
	// Port mappings
	PortMappings []PortMapping `json:"port_mappings"`
	// Private IP address
	PrivateIP string `json:"private_ip,nullable"`
	// Start timestamp (RFC3339)
	StartedAt time.Time `json:"started_at,nullable" format:"date-time"`
	// Stop timestamp (RFC3339)
	StoppedAt time.Time `json:"stopped_at,nullable" format:"date-time"`
	// Timeout configuration
	TimeoutSeconds int64 `json:"timeout_seconds"`
	// Number of virtual CPUs
	Vcpus int64 `json:"vcpus"`
	// Attached volumes
	Volumes []VolumeAttachment `json:"volumes"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		ID             respjson.Field
		CreatedAt      respjson.Field
		Image          respjson.Field
		Name           respjson.Field
		State          respjson.Field
		Env            respjson.Field
		Fqdn           respjson.Field
		HasSnapshot    respjson.Field
		MemoryMaxMB    respjson.Field
		MemoryMB       respjson.Field
		PortMappings   respjson.Field
		PrivateIP      respjson.Field
		StartedAt      respjson.Field
		StoppedAt      respjson.Field
		TimeoutSeconds respjson.Field
		Vcpus          respjson.Field
		Volumes        respjson.Field
		ExtraFields    map[string]respjson.Field
		raw            string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r Instance) RawJSON() string { return r.JSON.raw }
func (r *Instance) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

// Instance state:
//
// - Created: VMM created but not started (Cloud Hypervisor native)
// - Running: VM is actively running (Cloud Hypervisor native)
// - Paused: VM is paused (Cloud Hypervisor native)
// - Shutdown: VM shut down but VMM exists (Cloud Hypervisor native)
// - Stopped: No VMM running, no snapshot exists
// - Standby: No VMM running, snapshot exists (can be restored)
type InstanceState string

const (
	InstanceStateCreated  InstanceState = "Created"
	InstanceStateRunning  InstanceState = "Running"
	InstanceStatePaused   InstanceState = "Paused"
	InstanceStateShutdown InstanceState = "Shutdown"
	InstanceStateStopped  InstanceState = "Stopped"
	InstanceStateStandby  InstanceState = "Standby"
)

type PortMapping struct {
	// Port in the guest VM
	GuestPort int64 `json:"guest_port,required"`
	// Port on the host
	HostPort int64 `json:"host_port,required"`
	// Any of "tcp", "udp".
	Protocol PortMappingProtocol `json:"protocol"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		GuestPort   respjson.Field
		HostPort    respjson.Field
		Protocol    respjson.Field
		ExtraFields map[string]respjson.Field
		raw         string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r PortMapping) RawJSON() string { return r.JSON.raw }
func (r *PortMapping) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

// ToParam converts this PortMapping to a PortMappingParam.
//
// Warning: the fields of the param type will not be present. ToParam should only
// be used at the last possible moment before sending a request. Test for this with
// PortMappingParam.Overrides()
func (r PortMapping) ToParam() PortMappingParam {
	return param.Override[PortMappingParam](json.RawMessage(r.RawJSON()))
}

type PortMappingProtocol string

const (
	PortMappingProtocolTcp PortMappingProtocol = "tcp"
	PortMappingProtocolUdp PortMappingProtocol = "udp"
)

// The properties GuestPort, HostPort are required.
type PortMappingParam struct {
	// Port in the guest VM
	GuestPort int64 `json:"guest_port,required"`
	// Port on the host
	HostPort int64 `json:"host_port,required"`
	// Any of "tcp", "udp".
	Protocol PortMappingProtocol `json:"protocol,omitzero"`
	paramObj
}

func (r PortMappingParam) MarshalJSON() (data []byte, err error) {
	type shadow PortMappingParam
	return param.MarshalObject(r, (*shadow)(&r))
}
func (r *PortMappingParam) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type VolumeAttachment struct {
	// Path where volume is mounted in the guest
	MountPath string `json:"mount_path,required"`
	// Volume identifier
	VolumeID string `json:"volume_id,required"`
	// Whether volume is mounted read-only
	Readonly bool `json:"readonly"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		MountPath   respjson.Field
		VolumeID    respjson.Field
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

// ToParam converts this VolumeAttachment to a VolumeAttachmentParam.
//
// Warning: the fields of the param type will not be present. ToParam should only
// be used at the last possible moment before sending a request. Test for this with
// VolumeAttachmentParam.Overrides()
func (r VolumeAttachment) ToParam() VolumeAttachmentParam {
	return param.Override[VolumeAttachmentParam](json.RawMessage(r.RawJSON()))
}

// The properties MountPath, VolumeID are required.
type VolumeAttachmentParam struct {
	// Path where volume is mounted in the guest
	MountPath string `json:"mount_path,required"`
	// Volume identifier
	VolumeID string `json:"volume_id,required"`
	// Whether volume is mounted read-only
	Readonly param.Opt[bool] `json:"readonly,omitzero"`
	paramObj
}

func (r VolumeAttachmentParam) MarshalJSON() (data []byte, err error) {
	type shadow VolumeAttachmentParam
	return param.MarshalObject(r, (*shadow)(&r))
}
func (r *VolumeAttachmentParam) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type InstanceNewParams struct {
	// Unique identifier for the instance (provided by caller)
	ID string `json:"id,required"`
	// Image identifier
	Image string `json:"image,required"`
	// Human-readable name
	Name string `json:"name,required"`
	// Maximum memory with hotplug in MB
	MemoryMaxMB param.Opt[int64] `json:"memory_max_mb,omitzero"`
	// Base memory in MB
	MemoryMB param.Opt[int64] `json:"memory_mb,omitzero"`
	// Timeout for scale-to-zero semantics
	TimeoutSeconds param.Opt[int64] `json:"timeout_seconds,omitzero"`
	// Number of virtual CPUs
	Vcpus param.Opt[int64] `json:"vcpus,omitzero"`
	// Environment variables
	Env map[string]string `json:"env,omitzero"`
	// Port mappings from host to guest
	PortMappings []PortMappingParam `json:"port_mappings,omitzero"`
	// Volumes to attach
	Volumes []VolumeAttachmentParam `json:"volumes,omitzero"`
	paramObj
}

func (r InstanceNewParams) MarshalJSON() (data []byte, err error) {
	type shadow InstanceNewParams
	return param.MarshalObject(r, (*shadow)(&r))
}
func (r *InstanceNewParams) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type InstanceStreamLogsParams struct {
	// Follow logs (stream with SSE)
	Follow param.Opt[bool] `query:"follow,omitzero" json:"-"`
	// Number of lines to return from end
	Tail param.Opt[int64] `query:"tail,omitzero" json:"-"`
	paramObj
}

// URLQuery serializes [InstanceStreamLogsParams]'s query parameters as
// `url.Values`.
func (r InstanceStreamLogsParams) URLQuery() (v url.Values, err error) {
	return apiquery.MarshalWithSettings(r, apiquery.QuerySettings{
		ArrayFormat:  apiquery.ArrayQueryFormatComma,
		NestedFormat: apiquery.NestedQueryFormatBrackets,
	})
}
