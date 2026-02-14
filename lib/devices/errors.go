package devices

import "errors"

var (
	// ErrNotFound is returned when a device is not found
	ErrNotFound = errors.New("device not found")

	// ErrInUse is returned when a device is currently attached to an instance
	ErrInUse = errors.New("device is in use")

	// ErrNotBound is returned when a VFIO operation requires the device to be bound
	ErrNotBound = errors.New("device is not bound to VFIO")

	// ErrAlreadyBound is returned when trying to bind a device that's already bound to VFIO
	ErrAlreadyBound = errors.New("device is already bound to VFIO")

	// ErrAlreadyExists is returned when trying to register a device that already exists
	ErrAlreadyExists = errors.New("device already exists")

	// ErrInvalidName is returned when the device name doesn't match the required pattern
	ErrInvalidName = errors.New("device name must match pattern ^[a-zA-Z0-9][a-zA-Z0-9_.-]+$")

	// ErrNameExists is returned when a device with the same name already exists
	ErrNameExists = errors.New("device name already exists")

	// ErrInvalidPCIAddress is returned when the PCI address format is invalid
	ErrInvalidPCIAddress = errors.New("invalid PCI address format")

	// ErrDeviceNotFound is returned when the PCI device doesn't exist on the host
	ErrDeviceNotFound = errors.New("PCI device not found on host")

	// ErrVFIONotAvailable is returned when VFIO modules are not loaded
	ErrVFIONotAvailable = errors.New("VFIO is not available (modules not loaded)")

	// ErrIOMMUGroupConflict is returned when not all devices in IOMMU group can be passed through
	ErrIOMMUGroupConflict = errors.New("IOMMU group contains other devices that must also be passed through")
)


