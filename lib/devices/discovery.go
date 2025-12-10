package devices

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	sysfsDevicesPath = "/sys/bus/pci/devices"
	sysfsIOMMUPath   = "/sys/kernel/iommu_groups"
)

// pciAddressPattern matches PCI addresses like "0000:a2:00.0"
var pciAddressPattern = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]$`)

// ValidatePCIAddress validates that a string is a valid PCI address format
func ValidatePCIAddress(addr string) bool {
	return pciAddressPattern.MatchString(addr)
}

// DiscoverAvailableDevices scans sysfs for PCI devices that can be used for passthrough
// It filters for devices that are likely candidates (GPUs, network cards, etc.)
func DiscoverAvailableDevices() ([]AvailableDevice, error) {
	entries, err := os.ReadDir(sysfsDevicesPath)
	if err != nil {
		return nil, fmt.Errorf("read sysfs devices: %w", err)
	}

	var devices []AvailableDevice
	for _, entry := range entries {
		addr := entry.Name()
		if !ValidatePCIAddress(addr) {
			continue
		}

		device, err := readDeviceInfo(addr)
		if err != nil {
			// Skip devices we can't read
			continue
		}

		// Filter for passthrough-capable devices (GPUs, 3D controllers, etc.)
		if isPassthroughCandidate(device) {
			devices = append(devices, *device)
		}
	}

	return devices, nil
}

// GetDeviceInfo reads information about a specific PCI device
func GetDeviceInfo(pciAddress string) (*AvailableDevice, error) {
	if !ValidatePCIAddress(pciAddress) {
		return nil, ErrInvalidPCIAddress
	}

	devicePath := filepath.Join(sysfsDevicesPath, pciAddress)
	if _, err := os.Stat(devicePath); os.IsNotExist(err) {
		return nil, ErrDeviceNotFound
	}

	return readDeviceInfo(pciAddress)
}

// readDeviceInfo reads device information from sysfs
func readDeviceInfo(pciAddress string) (*AvailableDevice, error) {
	devicePath := filepath.Join(sysfsDevicesPath, pciAddress)

	vendorID, err := readSysfsFile(filepath.Join(devicePath, "vendor"))
	if err != nil {
		return nil, fmt.Errorf("read vendor: %w", err)
	}
	vendorID = strings.TrimPrefix(vendorID, "0x")

	deviceID, err := readSysfsFile(filepath.Join(devicePath, "device"))
	if err != nil {
		return nil, fmt.Errorf("read device: %w", err)
	}
	deviceID = strings.TrimPrefix(deviceID, "0x")

	iommuGroup, err := readIOMMUGroup(pciAddress)
	if err != nil {
		return nil, fmt.Errorf("read iommu group: %w", err)
	}

	driver := readCurrentDriver(pciAddress)

	// Get device class to determine type
	classCode, _ := readSysfsFile(filepath.Join(devicePath, "class"))

	return &AvailableDevice{
		PCIAddress:    pciAddress,
		VendorID:      vendorID,
		DeviceID:      deviceID,
		VendorName:    getVendorName(vendorID),
		DeviceName:    getDeviceName(vendorID, deviceID, classCode),
		IOMMUGroup:    iommuGroup,
		CurrentDriver: driver,
	}, nil
}

// readSysfsFile reads and trims a sysfs file
func readSysfsFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// readIOMMUGroup reads the IOMMU group number for a device
func readIOMMUGroup(pciAddress string) (int, error) {
	iommuLink := filepath.Join(sysfsDevicesPath, pciAddress, "iommu_group")
	target, err := os.Readlink(iommuLink)
	if err != nil {
		return -1, fmt.Errorf("read iommu_group link: %w", err)
	}

	// Target is like "../../../../kernel/iommu_groups/82"
	groupStr := filepath.Base(target)
	group, err := strconv.Atoi(groupStr)
	if err != nil {
		return -1, fmt.Errorf("parse iommu group: %w", err)
	}

	return group, nil
}

// readCurrentDriver reads the current driver bound to the device
func readCurrentDriver(pciAddress string) *string {
	driverLink := filepath.Join(sysfsDevicesPath, pciAddress, "driver")
	target, err := os.Readlink(driverLink)
	if err != nil {
		// No driver bound
		return nil
	}

	driver := filepath.Base(target)
	return &driver
}

// GetIOMMUGroupDevices returns all PCI devices in the same IOMMU group
func GetIOMMUGroupDevices(iommuGroup int) ([]string, error) {
	groupPath := filepath.Join(sysfsIOMMUPath, strconv.Itoa(iommuGroup), "devices")
	entries, err := os.ReadDir(groupPath)
	if err != nil {
		return nil, fmt.Errorf("read iommu group devices: %w", err)
	}

	var devices []string
	for _, entry := range entries {
		devices = append(devices, entry.Name())
	}
	return devices, nil
}

// isPassthroughCandidate determines if a device is a good candidate for passthrough
func isPassthroughCandidate(device *AvailableDevice) bool {
	// Check class code for GPUs and 3D controllers
	// Class 0x03 = Display controller
	// Subclass 0x00 = VGA controller
	// Subclass 0x02 = 3D controller (like NVIDIA compute GPUs)
	devicePath := filepath.Join(sysfsDevicesPath, device.PCIAddress)
	classCode, err := readSysfsFile(filepath.Join(devicePath, "class"))
	if err != nil {
		return false
	}

	classCode = strings.TrimPrefix(classCode, "0x")
	if len(classCode) >= 4 {
		classPrefix := classCode[:4]
		// 0300 = VGA controller, 0302 = 3D controller
		if classPrefix == "0300" || classPrefix == "0302" {
			return true
		}
	}

	// Also include NVIDIA devices by vendor ID
	if device.VendorID == "10de" {
		return true
	}

	return false
}

// getVendorName returns a human-readable vendor name
func getVendorName(vendorID string) string {
	vendors := map[string]string{
		"10de": "NVIDIA Corporation",
		"1002": "AMD/ATI",
		"8086": "Intel Corporation",
	}
	if name, ok := vendors[vendorID]; ok {
		return name
	}
	return "Unknown Vendor"
}

// getDeviceName returns a human-readable device name based on class and IDs
func getDeviceName(vendorID, deviceID, classCode string) string {
	// For NVIDIA, provide some common device names.
	// Sources:
	//   - NVIDIA Driver README, Appendix A "Supported NVIDIA GPU Products":
	//     https://download.nvidia.com/XFree86/Linux-x86_64/570.133.07/README/supportedchips.html
	//   - PCI ID Database: https://pci-ids.ucw.cz/read/PC/10de
	if vendorID == "10de" {
		nvidiaDevices := map[string]string{
			// H100 series
			"2321": "H100 NVL",
			"2330": "H100 SXM5 80GB",
			"2331": "H100 PCIe",
			"2339": "H100",
			// H200 series
			"2335": "H200",
			// L4
			"27b8": "L4",
			// L40 series
			"26b5": "L40",
			"26b9": "L40S",
			// A100 series
			"20b0": "A100 SXM4 40GB",
			"20b2": "A100 SXM4 80GB",
			"20b5": "A100 PCIe 40GB",
			"20f1": "A100 PCIe 80GB",
			// A30/A40
			"20b7": "A30",
			"2235": "A40",
			// RTX 4000 series (datacenter)
			"2684": "RTX 4090",
			"27b0": "RTX 4090 D",
			// V100 series
			"1db4": "V100 PCIe 16GB",
			"1db5": "V100 SXM2 16GB",
			"1db6": "V100 PCIe 32GB",
		}
		if name, ok := nvidiaDevices[deviceID]; ok {
			return name
		}
	}

	// Fall back to class-based description
	classCode = strings.TrimPrefix(classCode, "0x")
	if len(classCode) >= 4 {
		switch classCode[:4] {
		case "0300":
			return "VGA Controller"
		case "0302":
			return "3D Controller"
		case "0403":
			return "Audio Device"
		}
	}

	return "PCI Device"
}

// DetermineDeviceType determines the DeviceType based on device properties
func DetermineDeviceType(device *AvailableDevice) DeviceType {
	devicePath := filepath.Join(sysfsDevicesPath, device.PCIAddress)
	classCode, err := readSysfsFile(filepath.Join(devicePath, "class"))
	if err != nil {
		return DeviceTypeGeneric
	}

	classCode = strings.TrimPrefix(classCode, "0x")
	if len(classCode) >= 4 {
		classPrefix := classCode[:4]
		// 0300 = VGA controller, 0302 = 3D controller
		if classPrefix == "0300" || classPrefix == "0302" {
			return DeviceTypeGPU
		}
	}

	return DeviceTypeGeneric
}
