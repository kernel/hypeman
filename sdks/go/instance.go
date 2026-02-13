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

	"github.com/kernel/hypeman-go/internal/apijson"
	"github.com/kernel/hypeman-go/internal/apiquery"
	"github.com/kernel/hypeman-go/internal/requestconfig"
	"github.com/kernel/hypeman-go/option"
	"github.com/kernel/hypeman-go/packages/param"
	"github.com/kernel/hypeman-go/packages/respjson"
	"github.com/kernel/hypeman-go/packages/ssestream"
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

// Returns information about a path in the guest filesystem. Useful for checking if
// a path exists, its type, and permissions before performing file operations.
func (r *InstanceService) Stat(ctx context.Context, id string, query InstanceStatParams, opts ...option.RequestOption) (res *PathInfo, err error) {
	opts = slices.Concat(r.Options, opts)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("instances/%s/stat", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodGet, path, query, &res, opts...)
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
	// Disk I/O rate limit (human-readable, e.g., "100MB/s")
	DiskIoBps string `json:"disk_io_bps"`
	// Environment variables
	Env map[string]string `json:"env"`
	// GPU information attached to the instance
	GPU InstanceGPU `json:"gpu"`
	// Whether a snapshot exists for this instance
	HasSnapshot bool `json:"has_snapshot"`
	// Hotplug memory size (human-readable)
	HotplugSize string `json:"hotplug_size"`
	// Hypervisor running this instance
	//
	// Any of "cloud-hypervisor", "qemu", "vz".
	Hypervisor InstanceHypervisor `json:"hypervisor"`
	// User-defined key-value metadata
	Metadata map[string]string `json:"metadata"`
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
		DiskIoBps   respjson.Field
		Env         respjson.Field
		GPU         respjson.Field
		HasSnapshot respjson.Field
		HotplugSize respjson.Field
		Hypervisor  respjson.Field
		Metadata    respjson.Field
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

// GPU information attached to the instance
type InstanceGPU struct {
	// mdev device UUID
	MdevUuid string `json:"mdev_uuid"`
	// vGPU profile name
	Profile string `json:"profile"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		MdevUuid    respjson.Field
		Profile     respjson.Field
		ExtraFields map[string]respjson.Field
		raw         string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r InstanceGPU) RawJSON() string { return r.JSON.raw }
func (r *InstanceGPU) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

// Hypervisor running this instance
type InstanceHypervisor string

const (
	InstanceHypervisorCloudHypervisor InstanceHypervisor = "cloud-hypervisor"
	InstanceHypervisorQemu            InstanceHypervisor = "qemu"
	InstanceHypervisorVz              InstanceHypervisor = "vz"
)

// Network configuration of the instance
type InstanceNetwork struct {
	// Download bandwidth limit (human-readable, e.g., "1Gbps", "125MB/s")
	BandwidthDownload string `json:"bandwidth_download"`
	// Upload bandwidth limit (human-readable, e.g., "1Gbps", "125MB/s")
	BandwidthUpload string `json:"bandwidth_upload"`
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
		BandwidthDownload respjson.Field
		BandwidthUpload   respjson.Field
		Enabled           respjson.Field
		IP                respjson.Field
		Mac               respjson.Field
		Name              respjson.Field
		ExtraFields       map[string]respjson.Field
		raw               string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r InstanceNetwork) RawJSON() string { return r.JSON.raw }
func (r *InstanceNetwork) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type PathInfo struct {
	// Whether the path exists
	Exists bool `json:"exists,required"`
	// Error message if stat failed (e.g., permission denied). Only set when exists is
	// false due to an error rather than the path not existing.
	Error string `json:"error,nullable"`
	// True if this is a directory
	IsDir bool `json:"is_dir"`
	// True if this is a regular file
	IsFile bool `json:"is_file"`
	// True if this is a symbolic link (only set when follow_links=false)
	IsSymlink bool `json:"is_symlink"`
	// Symlink target path (only set when is_symlink=true)
	LinkTarget string `json:"link_target,nullable"`
	// File mode (Unix permissions)
	Mode int64 `json:"mode"`
	// File size in bytes
	Size int64 `json:"size"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		Exists      respjson.Field
		Error       respjson.Field
		IsDir       respjson.Field
		IsFile      respjson.Field
		IsSymlink   respjson.Field
		LinkTarget  respjson.Field
		Mode        respjson.Field
		Size        respjson.Field
		ExtraFields map[string]respjson.Field
		raw         string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r PathInfo) RawJSON() string { return r.JSON.raw }
func (r *PathInfo) UnmarshalJSON(data []byte) error {
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
	// Disk I/O rate limit (e.g., "100MB/s", "500MB/s"). Defaults to proportional share
	// based on CPU allocation if configured.
	DiskIoBps param.Opt[string] `json:"disk_io_bps,omitzero"`
	// Additional memory for hotplug (human-readable format like "3GB", "1G")
	HotplugSize param.Opt[string] `json:"hotplug_size,omitzero"`
	// Writable overlay disk size (human-readable format like "10GB", "50G")
	OverlaySize param.Opt[string] `json:"overlay_size,omitzero"`
	// Base memory size (human-readable format like "1GB", "512MB", "2G")
	Size param.Opt[string] `json:"size,omitzero"`
	// Skip guest-agent installation during boot. When true, the exec and stat APIs
	// will not work for this instance. The instance will still run, but remote command
	// execution will be unavailable.
	SkipGuestAgent param.Opt[bool] `json:"skip_guest_agent,omitzero"`
	// Skip kernel headers installation during boot for faster startup. When true, DKMS
	// (Dynamic Kernel Module Support) will not work, preventing compilation of
	// out-of-tree kernel modules (e.g., NVIDIA vGPU drivers). Recommended for
	// workloads that don't need kernel module compilation.
	SkipKernelHeaders param.Opt[bool] `json:"skip_kernel_headers,omitzero"`
	// Number of virtual CPUs
	Vcpus param.Opt[int64] `json:"vcpus,omitzero"`
	// Device IDs or names to attach for GPU/PCI passthrough
	Devices []string `json:"devices,omitzero"`
	// Environment variables
	Env map[string]string `json:"env,omitzero"`
	// GPU configuration for the instance
	GPU InstanceNewParamsGPU `json:"gpu,omitzero"`
	// Hypervisor to use for this instance. Defaults to server configuration.
	//
	// Any of "cloud-hypervisor", "qemu", "vz".
	Hypervisor InstanceNewParamsHypervisor `json:"hypervisor,omitzero"`
	// User-defined key-value metadata for the instance
	Metadata map[string]string `json:"metadata,omitzero"`
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

// GPU configuration for the instance
type InstanceNewParamsGPU struct {
	// vGPU profile name (e.g., "L40S-1Q"). Only used in vGPU mode.
	Profile param.Opt[string] `json:"profile,omitzero"`
	paramObj
}

func (r InstanceNewParamsGPU) MarshalJSON() (data []byte, err error) {
	type shadow InstanceNewParamsGPU
	return param.MarshalObject(r, (*shadow)(&r))
}
func (r *InstanceNewParamsGPU) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

// Hypervisor to use for this instance. Defaults to server configuration.
type InstanceNewParamsHypervisor string

const (
	InstanceNewParamsHypervisorCloudHypervisor InstanceNewParamsHypervisor = "cloud-hypervisor"
	InstanceNewParamsHypervisorQemu            InstanceNewParamsHypervisor = "qemu"
	InstanceNewParamsHypervisorVz              InstanceNewParamsHypervisor = "vz"
)

// Network configuration for the instance
type InstanceNewParamsNetwork struct {
	// Download bandwidth limit (external→VM, e.g., "1Gbps", "125MB/s"). Defaults to
	// proportional share based on CPU allocation.
	BandwidthDownload param.Opt[string] `json:"bandwidth_download,omitzero"`
	// Upload bandwidth limit (VM→external, e.g., "1Gbps", "125MB/s"). Defaults to
	// proportional share based on CPU allocation.
	BandwidthUpload param.Opt[string] `json:"bandwidth_upload,omitzero"`
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

type InstanceStatParams struct {
	// Path to stat in the guest filesystem
	Path string `query:"path,required" json:"-"`
	// Follow symbolic links (like stat vs lstat)
	FollowLinks param.Opt[bool] `query:"follow_links,omitzero" json:"-"`
	paramObj
}

// URLQuery serializes [InstanceStatParams]'s query parameters as `url.Values`.
func (r InstanceStatParams) URLQuery() (v url.Values, err error) {
	return apiquery.MarshalWithSettings(r, apiquery.QuerySettings{
		ArrayFormat:  apiquery.ArrayQueryFormatComma,
		NestedFormat: apiquery.NestedQueryFormatBrackets,
	})
}
