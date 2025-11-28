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
	Id          string     `json:"id"`           // cuid2 identifier
	Name        string     `json:"name"`         // user-provided globally unique name
	Type        DeviceType `json:"type"`         // gpu or pci
	PCIAddress  string     `json:"pci_address"`  // e.g., "0000:a2:00.0"
	VendorID    string     `json:"vendor_id"`    // e.g., "10de"
	DeviceID    string     `json:"device_id"`    // e.g., "27b8"
	IOMMUGroup  int        `json:"iommu_group"`  // IOMMU group number
	BoundToVFIO bool       `json:"bound_to_vfio"` // whether device is bound to vfio-pci
	AttachedTo  *string    `json:"attached_to"`  // instance ID if attached, nil otherwise
	CreatedAt   time.Time  `json:"created_at"`
}

// CreateDeviceRequest is the request to register a new device
type CreateDeviceRequest struct {
	Name       string `json:"name"`        // required: globally unique name
	PCIAddress string `json:"pci_address"` // required: PCI address (e.g., "0000:a2:00.0")
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


