package devices

import (
	"regexp"
	"time"
)

// DeviceType represents the type of PCI device
type DeviceType string

const (
	DeviceTypeGPU     DeviceType = "gpu"
	DeviceTypeGeneric DeviceType = "pci"
)

// Device represents a registered PCI device for passthrough
type Device struct {
	Id          string     `json:"id"`            // cuid2 identifier
	Name        string     `json:"name"`          // user-provided globally unique name
	Type        DeviceType `json:"type"`          // gpu or pci
	PCIAddress  string     `json:"pci_address"`   // e.g., "0000:a2:00.0"
	VendorID    string     `json:"vendor_id"`     // e.g., "10de"
	DeviceID    string     `json:"device_id"`     // e.g., "27b8"
	IOMMUGroup  int        `json:"iommu_group"`   // IOMMU group number
	BoundToVFIO bool       `json:"bound_to_vfio"` // whether device is bound to vfio-pci
	AttachedTo  *string    `json:"attached_to"`   // instance ID if attached, nil otherwise
	CreatedAt   time.Time  `json:"created_at"`
}

// CreateDeviceRequest is the request to register a new device
type CreateDeviceRequest struct {
	Name       string `json:"name,omitempty"` // optional: globally unique name (auto-generated if not provided)
	PCIAddress string `json:"pci_address"`    // required: PCI address (e.g., "0000:a2:00.0")
}

// AvailableDevice represents a PCI device discovered on the host
type AvailableDevice struct {
	PCIAddress    string  `json:"pci_address"`
	VendorID      string  `json:"vendor_id"`
	DeviceID      string  `json:"device_id"`
	VendorName    string  `json:"vendor_name"`
	DeviceName    string  `json:"device_name"`
	IOMMUGroup    int     `json:"iommu_group"`
	CurrentDriver *string `json:"current_driver"` // nil if no driver bound
}

// DeviceNamePattern is the regex pattern for valid device names
// Must start with alphanumeric, followed by alphanumeric, underscore, dot, or dash
var DeviceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

// ValidateDeviceName validates that a device name matches the required pattern
func ValidateDeviceName(name string) bool {
	return DeviceNamePattern.MatchString(name)
}

// GPUMode represents the host's GPU configuration mode
type GPUMode string

const (
	// GPUModePassthrough indicates whole GPU VFIO passthrough
	GPUModePassthrough GPUMode = "passthrough"
	// GPUModeVGPU indicates SR-IOV + mdev based vGPU
	GPUModeVGPU GPUMode = "vgpu"
	// GPUModeNone indicates no GPU available
	GPUModeNone GPUMode = "none"
)

// VirtualFunction represents an SR-IOV Virtual Function for vGPU
type VirtualFunction struct {
	PCIAddress string `json:"pci_address"` // e.g., "0000:82:00.4"
	ParentGPU  string `json:"parent_gpu"`  // e.g., "0000:82:00.0"
	HasMdev    bool   `json:"has_mdev"`    // true if an mdev is created on this VF
}

// MdevDevice represents an active mediated device (vGPU instance)
type MdevDevice struct {
	UUID        string `json:"uuid"`         // e.g., "aa618089-8b16-4d01-a136-25a0f3c73123"
	VFAddress   string `json:"vf_address"`   // VF this mdev resides on
	ProfileType string `json:"profile_type"` // internal type name, e.g., "nvidia-556"
	ProfileName string `json:"profile_name"` // user-facing name, e.g., "L40S-1Q"
	SysfsPath   string `json:"sysfs_path"`   // path for VMM device attachment
	InstanceID  string `json:"instance_id"`  // instance this mdev is attached to
}

// GPUProfile describes an available vGPU profile type
type GPUProfile struct {
	Name          string `json:"name"`           // user-facing name, e.g., "L40S-1Q"
	FramebufferMB int    `json:"framebuffer_mb"` // frame buffer size in MB
	Available     int    `json:"available"`      // number of VFs that can create this profile
}

// PassthroughDevice describes a physical GPU available for passthrough
type PassthroughDevice struct {
	Name      string `json:"name"`      // GPU name, e.g., "NVIDIA L40S"
	Available bool   `json:"available"` // true if not attached to an instance
}
