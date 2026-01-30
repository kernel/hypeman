// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

package hypeman

import (
	"context"
	"net/http"
	"slices"

	"github.com/kernel/hypeman-go/internal/apijson"
	"github.com/kernel/hypeman-go/internal/requestconfig"
	"github.com/kernel/hypeman-go/option"
	"github.com/kernel/hypeman-go/packages/respjson"
)

// ResourceService contains methods and other services that help with interacting
// with the hypeman API.
//
// Note, unlike clients, this service does not read variables from the environment
// automatically. You should not instantiate this service directly, and instead use
// the [NewResourceService] method instead.
type ResourceService struct {
	Options []option.RequestOption
}

// NewResourceService generates a new service that applies the given options to
// each request. These options are applied after the parent client's options (if
// there is one), and before any request-specific options.
func NewResourceService(opts ...option.RequestOption) (r ResourceService) {
	r = ResourceService{}
	r.Options = opts
	return
}

// Returns current host resource capacity, allocation status, and per-instance
// breakdown. Resources include CPU, memory, disk, and network. Oversubscription
// ratios are applied to calculate effective limits.
func (r *ResourceService) Get(ctx context.Context, opts ...option.RequestOption) (res *Resources, err error) {
	opts = slices.Concat(r.Options, opts)
	path := "resources"
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodGet, path, nil, &res, opts...)
	return
}

type DiskBreakdown struct {
	// Disk used by exported rootfs images
	ImagesBytes int64 `json:"images_bytes"`
	// Disk used by OCI layer cache (shared blobs)
	OciCacheBytes int64 `json:"oci_cache_bytes"`
	// Disk used by instance overlays (rootfs + volume overlays)
	OverlaysBytes int64 `json:"overlays_bytes"`
	// Disk used by volumes
	VolumesBytes int64 `json:"volumes_bytes"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		ImagesBytes   respjson.Field
		OciCacheBytes respjson.Field
		OverlaysBytes respjson.Field
		VolumesBytes  respjson.Field
		ExtraFields   map[string]respjson.Field
		raw           string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r DiskBreakdown) RawJSON() string { return r.JSON.raw }
func (r *DiskBreakdown) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

// Available vGPU profile
type GPUProfile struct {
	// Number of instances that can be created with this profile
	Available int64 `json:"available,required"`
	// Frame buffer size in MB
	FramebufferMB int64 `json:"framebuffer_mb,required"`
	// Profile name (user-facing)
	Name string `json:"name,required"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		Available     respjson.Field
		FramebufferMB respjson.Field
		Name          respjson.Field
		ExtraFields   map[string]respjson.Field
		raw           string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r GPUProfile) RawJSON() string { return r.JSON.raw }
func (r *GPUProfile) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

// GPU resource status. Null if no GPUs available.
type GPUResourceStatus struct {
	// GPU mode (vgpu for SR-IOV/mdev, passthrough for whole GPU)
	//
	// Any of "vgpu", "passthrough".
	Mode GPUResourceStatusMode `json:"mode,required"`
	// Total slots (VFs for vGPU, physical GPUs for passthrough)
	TotalSlots int64 `json:"total_slots,required"`
	// Slots currently in use
	UsedSlots int64 `json:"used_slots,required"`
	// Physical GPUs (only in passthrough mode)
	Devices []PassthroughDevice `json:"devices"`
	// Available vGPU profiles (only in vGPU mode)
	Profiles []GPUProfile `json:"profiles"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		Mode        respjson.Field
		TotalSlots  respjson.Field
		UsedSlots   respjson.Field
		Devices     respjson.Field
		Profiles    respjson.Field
		ExtraFields map[string]respjson.Field
		raw         string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r GPUResourceStatus) RawJSON() string { return r.JSON.raw }
func (r *GPUResourceStatus) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

// GPU mode (vgpu for SR-IOV/mdev, passthrough for whole GPU)
type GPUResourceStatusMode string

const (
	GPUResourceStatusModeVgpu        GPUResourceStatusMode = "vgpu"
	GPUResourceStatusModePassthrough GPUResourceStatusMode = "passthrough"
)

// Physical GPU available for passthrough
type PassthroughDevice struct {
	// Whether this GPU is available (not attached to an instance)
	Available bool `json:"available,required"`
	// GPU name
	Name string `json:"name,required"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		Available   respjson.Field
		Name        respjson.Field
		ExtraFields map[string]respjson.Field
		raw         string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r PassthroughDevice) RawJSON() string { return r.JSON.raw }
func (r *PassthroughDevice) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type ResourceAllocation struct {
	// vCPUs allocated
	CPU int64 `json:"cpu"`
	// Disk allocated in bytes (overlay + volumes)
	DiskBytes int64 `json:"disk_bytes"`
	// Disk I/O bandwidth limit in bytes/sec
	DiskIoBps int64 `json:"disk_io_bps"`
	// Instance identifier
	InstanceID string `json:"instance_id"`
	// Instance name
	InstanceName string `json:"instance_name"`
	// Memory allocated in bytes
	MemoryBytes int64 `json:"memory_bytes"`
	// Download bandwidth limit in bytes/sec (external→VM)
	NetworkDownloadBps int64 `json:"network_download_bps"`
	// Upload bandwidth limit in bytes/sec (VM→external)
	NetworkUploadBps int64 `json:"network_upload_bps"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		CPU                respjson.Field
		DiskBytes          respjson.Field
		DiskIoBps          respjson.Field
		InstanceID         respjson.Field
		InstanceName       respjson.Field
		MemoryBytes        respjson.Field
		NetworkDownloadBps respjson.Field
		NetworkUploadBps   respjson.Field
		ExtraFields        map[string]respjson.Field
		raw                string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r ResourceAllocation) RawJSON() string { return r.JSON.raw }
func (r *ResourceAllocation) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type ResourceStatus struct {
	// Currently allocated resources
	Allocated int64 `json:"allocated,required"`
	// Available for allocation (effective_limit - allocated)
	Available int64 `json:"available,required"`
	// Raw host capacity
	Capacity int64 `json:"capacity,required"`
	// Capacity after oversubscription (capacity \* ratio)
	EffectiveLimit int64 `json:"effective_limit,required"`
	// Oversubscription ratio applied
	OversubRatio float64 `json:"oversub_ratio,required"`
	// Resource type
	Type string `json:"type,required"`
	// How capacity was determined (detected, configured)
	Source string `json:"source"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		Allocated      respjson.Field
		Available      respjson.Field
		Capacity       respjson.Field
		EffectiveLimit respjson.Field
		OversubRatio   respjson.Field
		Type           respjson.Field
		Source         respjson.Field
		ExtraFields    map[string]respjson.Field
		raw            string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r ResourceStatus) RawJSON() string { return r.JSON.raw }
func (r *ResourceStatus) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type Resources struct {
	Allocations   []ResourceAllocation `json:"allocations,required"`
	CPU           ResourceStatus       `json:"cpu,required"`
	Disk          ResourceStatus       `json:"disk,required"`
	Memory        ResourceStatus       `json:"memory,required"`
	Network       ResourceStatus       `json:"network,required"`
	DiskBreakdown DiskBreakdown        `json:"disk_breakdown"`
	DiskIo        ResourceStatus       `json:"disk_io"`
	// GPU resource status. Null if no GPUs available.
	GPU GPUResourceStatus `json:"gpu,nullable"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		Allocations   respjson.Field
		CPU           respjson.Field
		Disk          respjson.Field
		Memory        respjson.Field
		Network       respjson.Field
		DiskBreakdown respjson.Field
		DiskIo        respjson.Field
		GPU           respjson.Field
		ExtraFields   map[string]respjson.Field
		raw           string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r Resources) RawJSON() string { return r.JSON.raw }
func (r *Resources) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}
