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

	"github.com/onkernel/hypeman-go/internal/apijson"
	"github.com/onkernel/hypeman-go/internal/apiquery"
	"github.com/onkernel/hypeman-go/internal/requestconfig"
	"github.com/onkernel/hypeman-go/option"
	"github.com/onkernel/hypeman-go/packages/param"
	"github.com/onkernel/hypeman-go/packages/respjson"
	"github.com/onkernel/hypeman-go/packages/ssestream"
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
	opts = append([]option.RequestOption{option.WithHeader("Accept", "*/*")}, opts...)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("instances/%s", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodDelete, path, nil, nil, opts...)
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

// Streams instance logs as Server-Sent Events. Use the `source` parameter to
// select which log to stream:
//
// - `app` (default): Guest application logs (serial console)
// - `vmm`: Cloud Hypervisor VMM logs
// - `hypeman`: Hypeman operations log
//
// Returns the last N lines (controlled by `tail` parameter), then optionally
// continues streaming new lines if `follow=true`.
func (r *InstanceService) LogsStreaming(ctx context.Context, id string, query InstanceLogsParams, opts ...option.RequestOption) (stream *ssestream.Stream[string]) {
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

// Restore instance from standby
func (r *InstanceService) Restore(ctx context.Context, id string, opts ...option.RequestOption) (res *Instance, err error) {
	opts = slices.Concat(r.Options, opts)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("instances/%s/restore", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodPost, path, nil, &res, opts...)
	return
}

// Put instance in standby (pause, snapshot, delete VMM)
func (r *InstanceService) Standby(ctx context.Context, id string, opts ...option.RequestOption) (res *Instance, err error) {
	opts = slices.Concat(r.Options, opts)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("instances/%s/standby", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodPost, path, nil, &res, opts...)
	return
}

// Start a stopped instance
func (r *InstanceService) Start(ctx context.Context, id string, opts ...option.RequestOption) (res *Instance, err error) {
	opts = slices.Concat(r.Options, opts)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("instances/%s/start", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodPost, path, nil, &res, opts...)
	return
}

// Stop instance (graceful shutdown)
func (r *InstanceService) Stop(ctx context.Context, id string, opts ...option.RequestOption) (res *Instance, err error) {
	opts = slices.Concat(r.Options, opts)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("instances/%s/stop", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodPost, path, nil, &res, opts...)
	return
}

type Instance struct {
	// Auto-generated unique identifier (CUID2 format)
	ID string `json:"id,required"`
	// Creation timestamp (RFC3339)
	CreatedAt time.Time `json:"created_at,required" format:"date-time"`
	// OCI image reference
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
	// - Unknown: Failed to determine state (see state_error for details)
	//
	// Any of "Created", "Running", "Paused", "Shutdown", "Stopped", "Standby",
	// "Unknown".
	State InstanceState `json:"state,required"`
	// Environment variables
	Env map[string]string `json:"env"`
	// Whether a snapshot exists for this instance
	HasSnapshot bool `json:"has_snapshot"`
	// Hotplug memory size (human-readable)
	HotplugSize string `json:"hotplug_size"`
	// Network configuration of the instance
	Network InstanceNetwork `json:"network"`
	// Writable overlay disk size (human-readable)
	OverlaySize string `json:"overlay_size"`
	// Base memory size (human-readable)
	Size string `json:"size"`
	// Start timestamp (RFC3339)
	StartedAt time.Time `json:"started_at,nullable" format:"date-time"`
	// Error message if state couldn't be determined (only set when state is Unknown)
	StateError string `json:"state_error,nullable"`
	// Stop timestamp (RFC3339)
	StoppedAt time.Time `json:"stopped_at,nullable" format:"date-time"`
	// Number of virtual CPUs
	Vcpus int64 `json:"vcpus"`
	// Volumes attached to the instance
	Volumes []VolumeMount `json:"volumes"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		ID          respjson.Field
		CreatedAt   respjson.Field
		Image       respjson.Field
		Name        respjson.Field
		State       respjson.Field
		Env         respjson.Field
		HasSnapshot respjson.Field
		HotplugSize respjson.Field
		Network     respjson.Field
		OverlaySize respjson.Field
		Size        respjson.Field
		StartedAt   respjson.Field
		StateError  respjson.Field
		StoppedAt   respjson.Field
		Vcpus       respjson.Field
		Volumes     respjson.Field
		ExtraFields map[string]respjson.Field
		raw         string
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
// - Unknown: Failed to determine state (see state_error for details)
type InstanceState string

const (
	InstanceStateCreated  InstanceState = "Created"
	InstanceStateRunning  InstanceState = "Running"
	InstanceStatePaused   InstanceState = "Paused"
	InstanceStateShutdown InstanceState = "Shutdown"
	InstanceStateStopped  InstanceState = "Stopped"
	InstanceStateStandby  InstanceState = "Standby"
	InstanceStateUnknown  InstanceState = "Unknown"
)

// Network configuration of the instance
type InstanceNetwork struct {
	// Whether instance is attached to the default network
	Enabled bool `json:"enabled"`
	// Assigned IP address (null if no network)
	IP string `json:"ip,nullable"`
	// Assigned MAC address (null if no network)
	Mac string `json:"mac,nullable"`
	// Network name (always "default" when enabled)
	Name string `json:"name"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		Enabled     respjson.Field
		IP          respjson.Field
		Mac         respjson.Field
		Name        respjson.Field
		ExtraFields map[string]respjson.Field
		raw         string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r InstanceNetwork) RawJSON() string { return r.JSON.raw }
func (r *InstanceNetwork) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type VolumeMount struct {
	// Path where volume is mounted in the guest
	MountPath string `json:"mount_path,required"`
	// Volume identifier
	VolumeID string `json:"volume_id,required"`
	// Create per-instance overlay for writes (requires readonly=true)
	Overlay bool `json:"overlay"`
	// Max overlay size as human-readable string (e.g., "1GB"). Required if
	// overlay=true.
	OverlaySize string `json:"overlay_size"`
	// Whether volume is mounted read-only
	Readonly bool `json:"readonly"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		MountPath   respjson.Field
		VolumeID    respjson.Field
		Overlay     respjson.Field
		OverlaySize respjson.Field
		Readonly    respjson.Field
		ExtraFields map[string]respjson.Field
		raw         string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r VolumeMount) RawJSON() string { return r.JSON.raw }
func (r *VolumeMount) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

// ToParam converts this VolumeMount to a VolumeMountParam.
//
// Warning: the fields of the param type will not be present. ToParam should only
// be used at the last possible moment before sending a request. Test for this with
// VolumeMountParam.Overrides()
func (r VolumeMount) ToParam() VolumeMountParam {
	return param.Override[VolumeMountParam](json.RawMessage(r.RawJSON()))
}

// The properties MountPath, VolumeID are required.
type VolumeMountParam struct {
	// Path where volume is mounted in the guest
	MountPath string `json:"mount_path,required"`
	// Volume identifier
	VolumeID string `json:"volume_id,required"`
	// Create per-instance overlay for writes (requires readonly=true)
	Overlay param.Opt[bool] `json:"overlay,omitzero"`
	// Max overlay size as human-readable string (e.g., "1GB"). Required if
	// overlay=true.
	OverlaySize param.Opt[string] `json:"overlay_size,omitzero"`
	// Whether volume is mounted read-only
	Readonly param.Opt[bool] `json:"readonly,omitzero"`
	paramObj
}

func (r VolumeMountParam) MarshalJSON() (data []byte, err error) {
	type shadow VolumeMountParam
	return param.MarshalObject(r, (*shadow)(&r))
}
func (r *VolumeMountParam) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type InstanceNewParams struct {
	// OCI image reference
	Image string `json:"image,required"`
	// Human-readable name (lowercase letters, digits, and dashes only; cannot start or
	// end with a dash)
	Name string `json:"name,required"`
	// Additional memory for hotplug (human-readable format like "3GB", "1G")
	HotplugSize param.Opt[string] `json:"hotplug_size,omitzero"`
	// Writable overlay disk size (human-readable format like "10GB", "50G")
	OverlaySize param.Opt[string] `json:"overlay_size,omitzero"`
	// Base memory size (human-readable format like "1GB", "512MB", "2G")
	Size param.Opt[string] `json:"size,omitzero"`
	// Number of virtual CPUs
	Vcpus param.Opt[int64] `json:"vcpus,omitzero"`
	// Environment variables
	Env map[string]string `json:"env,omitzero"`
	// Network configuration for the instance
	Network InstanceNewParamsNetwork `json:"network,omitzero"`
	// Volumes to attach to the instance at creation time
	Volumes []VolumeMountParam `json:"volumes,omitzero"`
	paramObj
}

func (r InstanceNewParams) MarshalJSON() (data []byte, err error) {
	type shadow InstanceNewParams
	return param.MarshalObject(r, (*shadow)(&r))
}
func (r *InstanceNewParams) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

// Network configuration for the instance
type InstanceNewParamsNetwork struct {
	// Whether to attach instance to the default network
	Enabled param.Opt[bool] `json:"enabled,omitzero"`
	paramObj
}

func (r InstanceNewParamsNetwork) MarshalJSON() (data []byte, err error) {
	type shadow InstanceNewParamsNetwork
	return param.MarshalObject(r, (*shadow)(&r))
}
func (r *InstanceNewParamsNetwork) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type InstanceLogsParams struct {
	// Continue streaming new lines after initial output
	Follow param.Opt[bool] `query:"follow,omitzero" json:"-"`
	// Number of lines to return from end
	Tail param.Opt[int64] `query:"tail,omitzero" json:"-"`
	// Log source to stream:
	//
	// - app: Guest application logs (serial console output)
	// - vmm: Cloud Hypervisor VMM logs (hypervisor stdout+stderr)
	// - hypeman: Hypeman operations log (actions taken on this instance)
	//
	// Any of "app", "vmm", "hypeman".
	Source InstanceLogsParamsSource `query:"source,omitzero" json:"-"`
	paramObj
}

// URLQuery serializes [InstanceLogsParams]'s query parameters as `url.Values`.
func (r InstanceLogsParams) URLQuery() (v url.Values, err error) {
	return apiquery.MarshalWithSettings(r, apiquery.QuerySettings{
		ArrayFormat:  apiquery.ArrayQueryFormatComma,
		NestedFormat: apiquery.NestedQueryFormatBrackets,
	})
}

// Log source to stream:
//
// - app: Guest application logs (serial console output)
// - vmm: Cloud Hypervisor VMM logs (hypervisor stdout+stderr)
// - hypeman: Hypeman operations log (actions taken on this instance)
type InstanceLogsParamsSource string

const (
	InstanceLogsParamsSourceApp     InstanceLogsParamsSource = "app"
	InstanceLogsParamsSourceVmm     InstanceLogsParamsSource = "vmm"
	InstanceLogsParamsSourceHypeman InstanceLogsParamsSource = "hypeman"
)
