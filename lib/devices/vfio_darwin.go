//go:build darwin

package devices

import (
	"fmt"
)

// ErrVFIONotSupportedOnMacOS is returned for VFIO operations on macOS
var ErrVFIONotSupportedOnMacOS = fmt.Errorf("VFIO device passthrough is not supported on macOS")

// VFIOBinder handles binding and unbinding devices to/from VFIO.
// On macOS, this is a stub that returns errors for all operations.
type VFIOBinder struct{}

// NewVFIOBinder creates a new VFIOBinder
func NewVFIOBinder() *VFIOBinder {
	return &VFIOBinder{}
}

// IsVFIOAvailable returns false on macOS as VFIO is not available.
func (v *VFIOBinder) IsVFIOAvailable() bool {
	return false
}

// IsDeviceBoundToVFIO returns false on macOS.
func (v *VFIOBinder) IsDeviceBoundToVFIO(pciAddress string) bool {
	return false
}

// BindToVFIO returns an error on macOS as VFIO is not supported.
func (v *VFIOBinder) BindToVFIO(pciAddress string) error {
	return ErrVFIONotSupportedOnMacOS
}

// UnbindFromVFIO returns an error on macOS as VFIO is not supported.
func (v *VFIOBinder) UnbindFromVFIO(pciAddress string) error {
	return ErrVFIONotSupportedOnMacOS
}

// GetVFIOGroupPath returns an error on macOS as VFIO is not supported.
func (v *VFIOBinder) GetVFIOGroupPath(pciAddress string) (string, error) {
	return "", ErrVFIONotSupportedOnMacOS
}

// CheckIOMMUGroupSafe returns an error on macOS as IOMMU is not available.
func (v *VFIOBinder) CheckIOMMUGroupSafe(pciAddress string, allowedDevices []string) error {
	return ErrVFIONotSupportedOnMacOS
}

// GetDeviceSysfsPath returns an empty string on macOS.
func GetDeviceSysfsPath(pciAddress string) string {
	return ""
}

// unbindFromDriver is not available on macOS.
func (v *VFIOBinder) unbindFromDriver(pciAddress, driver string) error {
	return ErrVFIONotSupportedOnMacOS
}

// setDriverOverride is not available on macOS.
func (v *VFIOBinder) setDriverOverride(pciAddress, driver string) error {
	return ErrVFIONotSupportedOnMacOS
}

// triggerDriverProbe is not available on macOS.
func (v *VFIOBinder) triggerDriverProbe(pciAddress string) error {
	return ErrVFIONotSupportedOnMacOS
}

// startNvidiaPersistenced is not available on macOS.
func (v *VFIOBinder) startNvidiaPersistenced() error {
	return nil // No-op, not an error
}
