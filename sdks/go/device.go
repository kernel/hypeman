// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

package hypeman

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/kernel/hypeman-go/internal/apijson"
	"github.com/kernel/hypeman-go/internal/requestconfig"
	"github.com/kernel/hypeman-go/option"
	"github.com/kernel/hypeman-go/packages/param"
	"github.com/kernel/hypeman-go/packages/respjson"
)

// DeviceService contains methods and other services that help with interacting
// with the hypeman API.
//
// Note, unlike clients, this service does not read variables from the environment
// automatically. You should not instantiate this service directly, and instead use
// the [NewDeviceService] method instead.
type DeviceService struct {
	Options []option.RequestOption
}

// NewDeviceService generates a new service that applies the given options to each
// request. These options are applied after the parent client's options (if there
// is one), and before any request-specific options.
func NewDeviceService(opts ...option.RequestOption) (r DeviceService) {
	r = DeviceService{}
	r.Options = opts
	return
}

// Register a device for passthrough
func (r *DeviceService) New(ctx context.Context, body DeviceNewParams, opts ...option.RequestOption) (res *Device, err error) {
	opts = slices.Concat(r.Options, opts)
	path := "devices"
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodPost, path, body, &res, opts...)
	return
}

// Get device details
func (r *DeviceService) Get(ctx context.Context, id string, opts ...option.RequestOption) (res *Device, err error) {
	opts = slices.Concat(r.Options, opts)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("devices/%s", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodGet, path, nil, &res, opts...)
	return
}

// List registered devices
func (r *DeviceService) List(ctx context.Context, opts ...option.RequestOption) (res *[]Device, err error) {
	opts = slices.Concat(r.Options, opts)
	path := "devices"
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodGet, path, nil, &res, opts...)
	return
}

// Unregister device
func (r *DeviceService) Delete(ctx context.Context, id string, opts ...option.RequestOption) (err error) {
	opts = slices.Concat(r.Options, opts)
	opts = append([]option.RequestOption{option.WithHeader("Accept", "*/*")}, opts...)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("devices/%s", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodDelete, path, nil, nil, opts...)
	return
}

// Discover passthrough-capable devices on host
func (r *DeviceService) ListAvailable(ctx context.Context, opts ...option.RequestOption) (res *[]AvailableDevice, err error) {
	opts = slices.Concat(r.Options, opts)
	path := "devices/available"
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodGet, path, nil, &res, opts...)
	return
}

type AvailableDevice struct {
	// PCI device ID (hex)
	DeviceID string `json:"device_id,required"`
	// IOMMU group number
	IommuGroup int64 `json:"iommu_group,required"`
	// PCI address
	PciAddress string `json:"pci_address,required"`
	// PCI vendor ID (hex)
	VendorID string `json:"vendor_id,required"`
	// Currently bound driver (null if none)
	CurrentDriver string `json:"current_driver,nullable"`
	// Human-readable device name
	DeviceName string `json:"device_name"`
	// Human-readable vendor name
	VendorName string `json:"vendor_name"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		DeviceID      respjson.Field
		IommuGroup    respjson.Field
		PciAddress    respjson.Field
		VendorID      respjson.Field
		CurrentDriver respjson.Field
		DeviceName    respjson.Field
		VendorName    respjson.Field
		ExtraFields   map[string]respjson.Field
		raw           string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r AvailableDevice) RawJSON() string { return r.JSON.raw }
func (r *AvailableDevice) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type Device struct {
	// Auto-generated unique identifier (CUID2 format)
	ID string `json:"id,required"`
	// Whether the device is currently bound to the vfio-pci driver, which is required
	// for VM passthrough.
	//
	//   - true: Device is bound to vfio-pci and ready for (or currently in use by) a VM.
	//     The device's native driver has been unloaded.
	//   - false: Device is using its native driver (e.g., nvidia) or no driver. Hypeman
	//     will automatically bind to vfio-pci when attaching to an instance.
	BoundToVfio bool `json:"bound_to_vfio,required"`
	// Registration timestamp (RFC3339)
	CreatedAt time.Time `json:"created_at,required" format:"date-time"`
	// PCI device ID (hex)
	DeviceID string `json:"device_id,required"`
	// IOMMU group number
	IommuGroup int64 `json:"iommu_group,required"`
	// PCI address
	PciAddress string `json:"pci_address,required"`
	// Type of PCI device
	//
	// Any of "gpu", "pci".
	Type DeviceType `json:"type,required"`
	// PCI vendor ID (hex)
	VendorID string `json:"vendor_id,required"`
	// Instance ID if attached
	AttachedTo string `json:"attached_to,nullable"`
	// Device name (user-provided or auto-generated from PCI address)
	Name string `json:"name"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		ID          respjson.Field
		BoundToVfio respjson.Field
		CreatedAt   respjson.Field
		DeviceID    respjson.Field
		IommuGroup  respjson.Field
		PciAddress  respjson.Field
		Type        respjson.Field
		VendorID    respjson.Field
		AttachedTo  respjson.Field
		Name        respjson.Field
		ExtraFields map[string]respjson.Field
		raw         string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r Device) RawJSON() string { return r.JSON.raw }
func (r *Device) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

// Type of PCI device
type DeviceType string

const (
	DeviceTypeGPU DeviceType = "gpu"
	DeviceTypePci DeviceType = "pci"
)

type DeviceNewParams struct {
	// PCI address of the device (required, e.g., "0000:a2:00.0")
	PciAddress string `json:"pci_address,required"`
	// Optional globally unique device name. If not provided, a name is auto-generated
	// from the PCI address (e.g., "pci-0000-a2-00-0")
	Name param.Opt[string] `json:"name,omitzero"`
	paramObj
}

func (r DeviceNewParams) MarshalJSON() (data []byte, err error) {
	type shadow DeviceNewParams
	return param.MarshalObject(r, (*shadow)(&r))
}
func (r *DeviceNewParams) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}
