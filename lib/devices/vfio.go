package devices

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	vfioDriverPath  = "/sys/bus/pci/drivers/vfio-pci"
	pciDriversPath  = "/sys/bus/pci/drivers"
	vfioDevicePath  = "/dev/vfio"
)

// VFIOBinder handles binding and unbinding devices to/from VFIO
type VFIOBinder struct{}

// NewVFIOBinder creates a new VFIOBinder
func NewVFIOBinder() *VFIOBinder {
	return &VFIOBinder{}
}

// IsVFIOAvailable checks if VFIO is available on the system
func (v *VFIOBinder) IsVFIOAvailable() bool {
	_, err := os.Stat(vfioDriverPath)
	return err == nil
}

// IsDeviceBoundToVFIO checks if a device is currently bound to vfio-pci
func (v *VFIOBinder) IsDeviceBoundToVFIO(pciAddress string) bool {
	driver := readCurrentDriver(pciAddress)
	return driver != nil && *driver == "vfio-pci"
}

// BindToVFIO binds a PCI device to the vfio-pci driver
// This requires:
// 1. Stopping any processes using the device (e.g., nvidia-persistenced for NVIDIA GPUs)
// 2. Unbinding the device from its current driver (if any)
// 3. Binding it to vfio-pci
func (v *VFIOBinder) BindToVFIO(pciAddress string) error {
	if !v.IsVFIOAvailable() {
		return ErrVFIONotAvailable
	}

	if v.IsDeviceBoundToVFIO(pciAddress) {
		return ErrAlreadyBound
	}

	// Get device info for vendor/device IDs
	deviceInfo, err := GetDeviceInfo(pciAddress)
	if err != nil {
		return fmt.Errorf("get device info: %w", err)
	}

	// For NVIDIA GPUs, stop nvidia-persistenced which holds the device open
	// This is required because the service keeps /dev/nvidia* open, blocking driver unbind
	isNvidia := deviceInfo.VendorID == "10de"
	stoppedNvidiaPersistenced := false
	if isNvidia {
		if err := v.stopNvidiaPersistenced(); err != nil {
			slog.Warn("failed to stop nvidia-persistenced", "error", err)
			// Continue anyway - it might not be running
		} else {
			stoppedNvidiaPersistenced = true
		}
	}

	// Use defer to ensure nvidia-persistenced is restarted on any error
	// after we successfully stopped it
	bindSucceeded := false
	defer func() {
		if stoppedNvidiaPersistenced && !bindSucceeded {
			_ = v.startNvidiaPersistenced()
		}
	}()

	// Unbind from current driver if bound
	currentDriver := readCurrentDriver(pciAddress)
	if currentDriver != nil && *currentDriver != "" {
		if err := v.unbindFromDriver(pciAddress, *currentDriver); err != nil {
			return fmt.Errorf("unbind from %s: %w", *currentDriver, err)
		}
	}

	// Override driver to vfio-pci
	if err := v.setDriverOverride(pciAddress, "vfio-pci"); err != nil {
		return fmt.Errorf("set driver override: %w", err)
	}

	// Bind to vfio-pci using the bind method (more reliable than new_id)
	if err := v.bindDeviceToVFIO(pciAddress); err != nil {
		return fmt.Errorf("bind to vfio-pci: %w", err)
	}

	bindSucceeded = true
	return nil
}

// UnbindFromVFIO unbinds a device from vfio-pci and restores the original driver
func (v *VFIOBinder) UnbindFromVFIO(pciAddress string) error {
	if !v.IsDeviceBoundToVFIO(pciAddress) {
		return ErrNotBound
	}

	// Get device info to check if it's NVIDIA
	deviceInfo, err := GetDeviceInfo(pciAddress)
	if err != nil {
		return fmt.Errorf("get device info: %w", err)
	}
	isNvidia := deviceInfo.VendorID == "10de"

	// Clear driver override first
	if err := v.setDriverOverride(pciAddress, ""); err != nil {
		// Non-fatal, continue with unbind
	}

	// Unbind from vfio-pci
	if err := v.unbindFromDriver(pciAddress, "vfio-pci"); err != nil {
		return fmt.Errorf("unbind from vfio-pci: %w", err)
	}

	// Trigger driver probe to rebind to original driver
	if err := v.triggerDriverProbe(pciAddress); err != nil {
		slog.Warn("failed to trigger driver probe", "pci_address", pciAddress, "error", err)
	}

	// For NVIDIA GPUs, restart nvidia-persistenced after rebinding
	if isNvidia {
		if err := v.startNvidiaPersistenced(); err != nil {
			slog.Warn("failed to start nvidia-persistenced", "error", err)
		}
	}

	return nil
}

// unbindFromDriver unbinds a device from its current driver
func (v *VFIOBinder) unbindFromDriver(pciAddress, driver string) error {
	unbindPath := filepath.Join(pciDriversPath, driver, "unbind")
	return os.WriteFile(unbindPath, []byte(pciAddress), 0200)
}

// setDriverOverride sets the driver_override for a device
func (v *VFIOBinder) setDriverOverride(pciAddress, driver string) error {
	overridePath := filepath.Join(sysfsDevicesPath, pciAddress, "driver_override")
	
	// Empty string clears the override
	content := driver
	if driver == "" {
		content = "\n" // Writing newline clears the override
	}
	
	return os.WriteFile(overridePath, []byte(content), 0200)
}


// bindDeviceToVFIO binds a specific device to vfio-pci using bind
func (v *VFIOBinder) bindDeviceToVFIO(pciAddress string) error {
	bindPath := filepath.Join(vfioDriverPath, "bind")
	return os.WriteFile(bindPath, []byte(pciAddress), 0200)
}

// triggerDriverProbe triggers the kernel to probe for drivers for a device
func (v *VFIOBinder) triggerDriverProbe(pciAddress string) error {
	probePath := "/sys/bus/pci/drivers_probe"
	return os.WriteFile(probePath, []byte(pciAddress), 0200)
}

// stopNvidiaPersistenced stops the nvidia-persistenced service
// This service keeps /dev/nvidia* open and blocks driver unbind
func (v *VFIOBinder) stopNvidiaPersistenced() error {
	slog.Debug("stopping nvidia-persistenced service")
	
	// Try systemctl first (works as root)
	cmd := exec.Command("systemctl", "stop", "nvidia-persistenced")
	if err := cmd.Run(); err == nil {
		return nil
	}
	
	// Fall back to killing the process directly (works with CAP_KILL or as root)
	// This is less clean but allows running with capabilities instead of full root
	cmd = exec.Command("pkill", "-TERM", "nvidia-persistenced")
	if err := cmd.Run(); err != nil {
		// Check if process even exists
		checkCmd := exec.Command("pgrep", "nvidia-persistenced")
		if checkCmd.Run() != nil {
			// Process doesn't exist, that's fine
			return nil
		}
		return fmt.Errorf("failed to stop nvidia-persistenced (try: sudo systemctl stop nvidia-persistenced)")
	}
	
	// Wait for process to exit with polling instead of arbitrary sleep
	return v.waitForProcessExit("nvidia-persistenced", 2*time.Second)
}

// waitForProcessExit polls for a process to exit, with timeout
func (v *VFIOBinder) waitForProcessExit(processName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	pollInterval := 100 * time.Millisecond
	
	for time.Now().Before(deadline) {
		checkCmd := exec.Command("pgrep", processName)
		if checkCmd.Run() != nil {
			// Process no longer exists
			return nil
		}
		time.Sleep(pollInterval)
	}
	
	// Timeout - process still running
	slog.Warn("timeout waiting for process to exit", "process", processName, "timeout", timeout)
	return nil // Continue anyway, the bind might still work
}

// startNvidiaPersistenced starts the nvidia-persistenced service
func (v *VFIOBinder) startNvidiaPersistenced() error {
	slog.Debug("starting nvidia-persistenced service")
	
	// Try systemctl first (works as root)
	cmd := exec.Command("systemctl", "start", "nvidia-persistenced")
	if err := cmd.Run(); err != nil {
		// If we can't start it, just log - not critical for test cleanup
		slog.Warn("could not restart nvidia-persistenced", "error", err)
	}
	return nil
}

// GetVFIOGroupPath returns the path to the VFIO group device for a PCI device
func (v *VFIOBinder) GetVFIOGroupPath(pciAddress string) (string, error) {
	iommuGroup, err := readIOMMUGroup(pciAddress)
	if err != nil {
		return "", fmt.Errorf("read iommu group: %w", err)
	}

	groupPath := filepath.Join(vfioDevicePath, fmt.Sprintf("%d", iommuGroup))
	if _, err := os.Stat(groupPath); os.IsNotExist(err) {
		return "", fmt.Errorf("vfio group device not found: %s", groupPath)
	}

	return groupPath, nil
}

// CheckIOMMUGroupSafe checks if all devices in the IOMMU group are safe to pass through
// Returns an error if there are other devices in the group that aren't being passed through
func (v *VFIOBinder) CheckIOMMUGroupSafe(pciAddress string, allowedDevices []string) error {
	iommuGroup, err := readIOMMUGroup(pciAddress)
	if err != nil {
		return fmt.Errorf("read iommu group: %w", err)
	}

	groupDevices, err := GetIOMMUGroupDevices(iommuGroup)
	if err != nil {
		return fmt.Errorf("get iommu group devices: %w", err)
	}

	// Build a set of allowed devices
	allowed := make(map[string]bool)
	for _, addr := range allowedDevices {
		allowed[addr] = true
	}

	// Check each device in the group
	for _, device := range groupDevices {
		if allowed[device] {
			continue
		}

		// Check if device is already bound to vfio-pci or is a bridge
		driver := readCurrentDriver(device)
		if driver != nil && *driver == "vfio-pci" {
			continue
		}

		// Check if it's a PCI bridge (these are usually okay to leave)
		if v.isPCIBridge(device) {
			continue
		}

		// Found a device that's not allowed and not safe
		return fmt.Errorf("%w: device %s in IOMMU group %d is not included",
			ErrIOMMUGroupConflict, device, iommuGroup)
	}

	return nil
}

// isPCIBridge checks if a device is a PCI bridge
func (v *VFIOBinder) isPCIBridge(pciAddress string) bool {
	classPath := filepath.Join(sysfsDevicesPath, pciAddress, "class")
	classCode, err := readSysfsFile(classPath)
	if err != nil {
		return false
	}

	classCode = strings.TrimPrefix(classCode, "0x")
	// Class 06 = Bridge, Subclass 04 = PCI bridge
	return len(classCode) >= 4 && classCode[:2] == "06"
}

// GetDeviceSysfsPath returns the sysfs path for a PCI device (used by cloud-hypervisor)
func GetDeviceSysfsPath(pciAddress string) string {
	return filepath.Join(sysfsDevicesPath, pciAddress) + "/"
}


