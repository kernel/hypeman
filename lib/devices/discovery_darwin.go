//go:build darwin

package devices

import (
	"fmt"
)

// ErrNotSupportedOnMacOS is returned for operations not supported on macOS
var ErrNotSupportedOnMacOS = fmt.Errorf("PCI device passthrough is not supported on macOS")

// ValidatePCIAddress validates that a string is a valid PCI address format.
// On macOS, this always returns false as PCI passthrough is not supported.
func ValidatePCIAddress(addr string) bool {
	return false
}

// DiscoverAvailableDevices returns an empty list on macOS.
// PCI device passthrough is not supported on macOS.
func DiscoverAvailableDevices() ([]AvailableDevice, error) {
	return []AvailableDevice{}, nil
}

// GetDeviceInfo returns an error on macOS as PCI passthrough is not supported.
func GetDeviceInfo(pciAddress string) (*AvailableDevice, error) {
	return nil, ErrNotSupportedOnMacOS
}

// GetIOMMUGroupDevices returns an error on macOS as IOMMU is not available.
func GetIOMMUGroupDevices(iommuGroup int) ([]string, error) {
	return nil, ErrNotSupportedOnMacOS
}

// DetermineDeviceType returns DeviceTypeGeneric on macOS.
func DetermineDeviceType(device *AvailableDevice) DeviceType {
	return DeviceTypeGeneric
}

// readSysfsFile is not available on macOS.
func readSysfsFile(path string) (string, error) {
	return "", ErrNotSupportedOnMacOS
}

// readIOMMUGroup is not available on macOS.
func readIOMMUGroup(pciAddress string) (int, error) {
	return -1, ErrNotSupportedOnMacOS
}

// readCurrentDriver is not available on macOS.
func readCurrentDriver(pciAddress string) *string {
	return nil
}
