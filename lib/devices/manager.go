package devices

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nrednav/cuid2"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/paths"
)

// InstanceLivenessChecker provides a way to check if an instance is running.
// This interface allows devices to query instance state without a circular dependency.
type InstanceLivenessChecker interface {
	// IsInstanceRunning returns true if the instance exists and is in a running state
	// (i.e., has an active VMM process). Returns false if the instance doesn't exist
	// or is stopped/standby/unknown.
	IsInstanceRunning(ctx context.Context, instanceID string) bool

	// GetInstanceDevices returns the list of device IDs attached to an instance.
	// Returns nil if the instance doesn't exist.
	GetInstanceDevices(ctx context.Context, instanceID string) []string

	// ListAllInstanceDevices returns a map of instanceID -> []deviceIDs for all instances.
	ListAllInstanceDevices(ctx context.Context) map[string][]string

	// DetectSuspiciousVMMProcesses finds cloud-hypervisor processes that don't match
	// known instances and logs warnings. Returns the count of suspicious processes found.
	DetectSuspiciousVMMProcesses(ctx context.Context) int
}

// Manager provides device management operations
type Manager interface {
	// ListDevices returns all registered devices
	ListDevices(ctx context.Context) ([]Device, error)

	// ListAvailableDevices discovers passthrough-capable devices on the host
	ListAvailableDevices(ctx context.Context) ([]AvailableDevice, error)

	// CreateDevice registers a new device for passthrough
	CreateDevice(ctx context.Context, req CreateDeviceRequest) (*Device, error)

	// GetDevice returns a device by ID or name
	GetDevice(ctx context.Context, idOrName string) (*Device, error)

	// DeleteDevice unregisters a device
	DeleteDevice(ctx context.Context, id string) error

	// BindToVFIO binds a device to vfio-pci driver
	BindToVFIO(ctx context.Context, id string) error

	// UnbindFromVFIO unbinds a device from vfio-pci driver
	UnbindFromVFIO(ctx context.Context, id string) error

	// MarkAttached marks a device as attached to an instance
	MarkAttached(ctx context.Context, deviceID, instanceID string) error

	// MarkDetached marks a device as detached from an instance
	MarkDetached(ctx context.Context, deviceID string) error

	// ReconcileDevices cleans up stale device state on startup.
	// It detects devices with AttachedTo referencing non-existent instances
	// and clears the orphaned attachment state.
	ReconcileDevices(ctx context.Context) error

	// SetLivenessChecker sets the instance liveness checker after construction.
	// This allows breaking the circular dependency between device and instance managers.
	SetLivenessChecker(checker InstanceLivenessChecker)
}

type manager struct {
	paths           *paths.Paths
	vfioBinder      *VFIOBinder
	livenessChecker InstanceLivenessChecker
	mu              sync.RWMutex
}

// NewManager creates a new device manager.
// Use SetLivenessChecker after construction to enable accurate orphan detection.
func NewManager(p *paths.Paths) Manager {
	return &manager{
		paths:      p,
		vfioBinder: NewVFIOBinder(),
	}
}

// SetLivenessChecker sets the instance liveness checker.
// This enables accurate orphan detection during reconciliation.
// If not set, orphan detection falls back to checking if the instance directory exists.
func (m *manager) SetLivenessChecker(checker InstanceLivenessChecker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.livenessChecker = checker
}

func (m *manager) ListDevices(ctx context.Context) ([]Device, error) {
	// RLock protects against concurrent directory modifications (CreateDevice/DeleteDevice)
	// during iteration. While individual file reads are atomic, directory iteration could
	// see inconsistent state if a device is being created or deleted concurrently.
	m.mu.RLock()
	defer m.mu.RUnlock()

	entries, err := os.ReadDir(m.paths.DevicesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []Device{}, nil
		}
		return nil, fmt.Errorf("read devices dir: %w", err)
	}

	var devices []Device
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		device, err := m.loadDevice(entry.Name())
		if err != nil {
			continue
		}

		// Update VFIO binding status from system state
		device.BoundToVFIO = m.vfioBinder.IsDeviceBoundToVFIO(device.PCIAddress)

		devices = append(devices, *device)
	}

	return devices, nil
}

func (m *manager) ListAvailableDevices(ctx context.Context) ([]AvailableDevice, error) {
	return DiscoverAvailableDevices()
}

func (m *manager) CreateDevice(ctx context.Context, req CreateDeviceRequest) (*Device, error) {
	log := logger.FromContext(ctx)

	// Validate PCI address format (required)
	if !ValidatePCIAddress(req.PCIAddress) {
		return nil, ErrInvalidPCIAddress
	}

	// Get device info from sysfs
	deviceInfo, err := GetDeviceInfo(req.PCIAddress)
	if err != nil {
		return nil, fmt.Errorf("get device info: %w", err)
	}

	// Generate ID
	id := cuid2.Generate()

	// Handle optional name: if not provided, generate one from PCI address
	name := req.Name
	if name == "" {
		// Generate name from PCI address: 0000:a2:00.0 -> pci-0000-a2-00-0
		name = "pci-" + strings.ReplaceAll(strings.ReplaceAll(req.PCIAddress, ":", "-"), ".", "-")
	}

	// Validate name format
	if !ValidateDeviceName(name) {
		return nil, ErrInvalidName
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if name already exists
	if _, err := m.findByName(name); err == nil {
		return nil, ErrNameExists
	}

	// Check if PCI address already registered
	if _, err := m.findByPCIAddress(req.PCIAddress); err == nil {
		return nil, ErrAlreadyExists
	}

	// Create device
	device := &Device{
		Id:          id,
		Name:        name,
		Type:        DetermineDeviceType(deviceInfo),
		PCIAddress:  req.PCIAddress,
		VendorID:    deviceInfo.VendorID,
		DeviceID:    deviceInfo.DeviceID,
		IOMMUGroup:  deviceInfo.IOMMUGroup,
		BoundToVFIO: m.vfioBinder.IsDeviceBoundToVFIO(req.PCIAddress),
		AttachedTo:  nil,
		CreatedAt:   time.Now(),
	}

	// Ensure directories exist
	if err := os.MkdirAll(m.paths.DeviceDir(id), 0755); err != nil {
		return nil, fmt.Errorf("create device dir: %w", err)
	}

	// Save device metadata
	if err := m.saveDevice(device); err != nil {
		os.RemoveAll(m.paths.DeviceDir(id))
		return nil, fmt.Errorf("save device: %w", err)
	}

	log.InfoContext(ctx, "registered device",
		"id", id,
		"name", name,
		"pci_address", req.PCIAddress,
		"type", device.Type,
	)

	return device, nil
}

func (m *manager) GetDevice(ctx context.Context, idOrName string) (*Device, error) {
	// RLock protects against concurrent modifications while looking up by name,
	// which requires iterating the devices directory.
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Try by ID first
	device, err := m.loadDevice(idOrName)
	if err == nil {
		device.BoundToVFIO = m.vfioBinder.IsDeviceBoundToVFIO(device.PCIAddress)
		return device, nil
	}

	// Try by name
	device, err = m.findByName(idOrName)
	if err == nil {
		device.BoundToVFIO = m.vfioBinder.IsDeviceBoundToVFIO(device.PCIAddress)
		return device, nil
	}

	return nil, ErrNotFound
}

func (m *manager) DeleteDevice(ctx context.Context, id string) error {
	log := logger.FromContext(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	device, err := m.loadDevice(id)
	if err != nil {
		// Try by name
		device, err = m.findByName(id)
		if err != nil {
			return ErrNotFound
		}
		id = device.Id
	}

	// Check if device is attached
	if device.AttachedTo != nil {
		return ErrInUse
	}

	// Remove device directory
	if err := os.RemoveAll(m.paths.DeviceDir(id)); err != nil {
		return fmt.Errorf("remove device dir: %w", err)
	}

	log.InfoContext(ctx, "unregistered device",
		"id", id,
		"name", device.Name,
		"pci_address", device.PCIAddress,
	)

	return nil
}

func (m *manager) BindToVFIO(ctx context.Context, id string) error {
	log := logger.FromContext(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	device, err := m.loadDevice(id)
	if err != nil {
		// Try by name
		device, err = m.findByName(id)
		if err != nil {
			return ErrNotFound
		}
	}

	// Check IOMMU group safety
	if err := m.vfioBinder.CheckIOMMUGroupSafe(device.PCIAddress, []string{device.PCIAddress}); err != nil {
		return err
	}

	// Bind to VFIO
	if err := m.vfioBinder.BindToVFIO(device.PCIAddress); err != nil {
		return err
	}

	// Update device state
	device.BoundToVFIO = true
	if err := m.saveDevice(device); err != nil {
		return fmt.Errorf("save device: %w", err)
	}

	log.InfoContext(ctx, "bound device to VFIO",
		"id", device.Id,
		"name", device.Name,
		"pci_address", device.PCIAddress,
	)

	return nil
}

func (m *manager) UnbindFromVFIO(ctx context.Context, id string) error {
	log := logger.FromContext(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	device, err := m.loadDevice(id)
	if err != nil {
		// Try by name
		device, err = m.findByName(id)
		if err != nil {
			return ErrNotFound
		}
	}

	// Check if device is attached
	if device.AttachedTo != nil {
		return ErrInUse
	}

	// Unbind from VFIO
	if err := m.vfioBinder.UnbindFromVFIO(device.PCIAddress); err != nil {
		return err
	}

	// Update device state
	device.BoundToVFIO = false
	if err := m.saveDevice(device); err != nil {
		return fmt.Errorf("save device: %w", err)
	}

	log.InfoContext(ctx, "unbound device from VFIO",
		"id", device.Id,
		"name", device.Name,
		"pci_address", device.PCIAddress,
	)

	return nil
}

func (m *manager) MarkAttached(ctx context.Context, deviceID, instanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	device, err := m.loadDevice(deviceID)
	if err != nil {
		device, err = m.findByName(deviceID)
		if err != nil {
			return ErrNotFound
		}
	}

	if device.AttachedTo != nil {
		return ErrInUse
	}

	device.AttachedTo = &instanceID
	return m.saveDevice(device)
}

func (m *manager) MarkDetached(ctx context.Context, deviceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	device, err := m.loadDevice(deviceID)
	if err != nil {
		device, err = m.findByName(deviceID)
		if err != nil {
			return ErrNotFound
		}
	}

	device.AttachedTo = nil
	return m.saveDevice(device)
}

// ReconcileDevices cleans up stale device state on startup.
// It performs safe-by-default reconciliation:
// 1. Detects orphaned device attachments (instance missing or not running)
// 2. Clears orphaned AttachedTo metadata
// 3. Runs GPU-reset-lite for orphaned devices (unbind VFIO, clear override, probe driver)
// 4. Logs mismatches between instance→device and device→instance references
// 5. Detects suspicious cloud-hypervisor processes
func (m *manager) ReconcileDevices(ctx context.Context) error {
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "reconciling device state")

	// Validate GPU prerequisites and log warnings
	m.validatePrerequisites(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	entries, err := os.ReadDir(m.paths.DevicesDir())
	if err != nil {
		if os.IsNotExist(err) {
			// No devices directory yet, nothing to reconcile
			return nil
		}
		return fmt.Errorf("read devices dir: %w", err)
	}

	// Load all devices
	var allDevices []*Device
	deviceByID := make(map[string]*Device)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		device, err := m.loadDevice(entry.Name())
		if err != nil {
			log.WarnContext(ctx, "failed to load device during reconciliation",
				"device_id", entry.Name(),
				"error", err,
			)
			continue
		}
		// Update VFIO binding status from system state
		device.BoundToVFIO = m.vfioBinder.IsDeviceBoundToVFIO(device.PCIAddress)
		allDevices = append(allDevices, device)
		deviceByID[device.Id] = device
	}

	// Build instance→device map if we have a liveness checker
	var instanceDevices map[string][]string
	if m.livenessChecker != nil {
		instanceDevices = m.livenessChecker.ListAllInstanceDevices(ctx)
	}

	// Track stats
	var stats reconcileStats

	// Phase 1: Detect and handle orphaned device attachments
	for _, device := range allDevices {
		if device.AttachedTo == nil {
			continue
		}

		instanceID := *device.AttachedTo
		orphaned := m.isInstanceOrphaned(ctx, instanceID)

		if orphaned {
			log.WarnContext(ctx, "detected orphaned device attachment",
				"device_id", device.Id,
				"device_name", device.Name,
				"pci_address", device.PCIAddress,
				"orphaned_instance_id", instanceID,
			)

			// Clear the orphaned attachment
			device.AttachedTo = nil
			if err := m.saveDevice(device); err != nil {
				log.ErrorContext(ctx, "failed to save device after clearing attachment",
					"device_id", device.Id,
					"error", err,
				)
				stats.errors++
				continue
			}
			stats.orphanedCleared++

			// Run GPU-reset-lite for orphaned device
			m.resetOrphanedDevice(ctx, device, &stats)
		}
	}

	// Phase 2: Two-way reconciliation (log-only for mismatches)
	if instanceDevices != nil {
		for instanceID, deviceIDs := range instanceDevices {
			for _, deviceID := range deviceIDs {
				device, exists := deviceByID[deviceID]
				if !exists {
					// Instance references a device that doesn't exist in device metadata
					log.WarnContext(ctx, "instance references unknown device (mismatch)",
						"instance_id", instanceID,
						"device_id", deviceID,
					)
					stats.mismatches++
					continue
				}

				// Check if device's AttachedTo matches
				if device.AttachedTo == nil {
					log.WarnContext(ctx, "instance references device but device.AttachedTo is nil (mismatch)",
						"instance_id", instanceID,
						"device_id", deviceID,
						"device_name", device.Name,
					)
					stats.mismatches++
				} else if *device.AttachedTo != instanceID {
					log.WarnContext(ctx, "instance references device but device.AttachedTo points elsewhere (mismatch)",
						"instance_id", instanceID,
						"device_id", deviceID,
						"device_name", device.Name,
						"device_attached_to", *device.AttachedTo,
					)
					stats.mismatches++
				}

				// Check VFIO binding state - if instance is running, device should be bound
				if m.livenessChecker != nil && m.livenessChecker.IsInstanceRunning(ctx, instanceID) {
					if !device.BoundToVFIO {
						log.WarnContext(ctx, "running instance has device not bound to VFIO (mismatch)",
							"instance_id", instanceID,
							"device_id", deviceID,
							"device_name", device.Name,
							"pci_address", device.PCIAddress,
						)
						stats.mismatches++
					}
				}
			}
		}
	}

	// Phase 3: Detect suspicious cloud-hypervisor processes (log-only)
	if m.livenessChecker != nil {
		stats.suspiciousVMM = m.livenessChecker.DetectSuspiciousVMMProcesses(ctx)
	}

	// Log summary
	log.InfoContext(ctx, "device reconciliation complete",
		"orphaned_cleared", stats.orphanedCleared,
		"reset_attempted", stats.resetAttempted,
		"reset_succeeded", stats.resetSucceeded,
		"reset_failed", stats.resetFailed,
		"mismatches", stats.mismatches,
		"suspicious_vmm", stats.suspiciousVMM,
		"errors", stats.errors,
	)

	return nil
}

// validatePrerequisites checks GPU passthrough prerequisites and logs warnings.
// This helps operators debug configuration issues.
func (m *manager) validatePrerequisites(ctx context.Context) {
	log := logger.FromContext(ctx)

	// Check IOMMU availability
	iommuGroupsDir := "/sys/kernel/iommu_groups"
	entries, err := os.ReadDir(iommuGroupsDir)
	if err != nil {
		log.WarnContext(ctx, "IOMMU not available - GPU passthrough will not work",
			"error", err,
			"hint", "enable IOMMU in BIOS and kernel (intel_iommu=on or amd_iommu=on)",
		)
	} else if len(entries) == 0 {
		log.WarnContext(ctx, "no IOMMU groups found - GPU passthrough will not work",
			"hint", "enable IOMMU in BIOS and kernel (intel_iommu=on or amd_iommu=on)",
		)
	}

	// Check VFIO modules
	vfioModules := []string{"vfio_pci", "vfio_iommu_type1"}
	for _, module := range vfioModules {
		modulePath := "/sys/module/" + module
		if _, err := os.Stat(modulePath); os.IsNotExist(err) {
			log.WarnContext(ctx, "VFIO module not loaded - GPU passthrough will not work",
				"module", module,
				"hint", "run: modprobe "+module,
			)
		}
	}

	// Check huge pages (info-level hint if devices exist but no huge pages)
	hugePagesPath := "/sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages"
	if data, err := os.ReadFile(hugePagesPath); err == nil {
		count := strings.TrimSpace(string(data))
		if count == "0" || count == "" {
			// Only warn if we have registered devices
			if devicesDir := m.paths.DevicesDir(); devicesDir != "" {
				if entries, err := os.ReadDir(devicesDir); err == nil && len(entries) > 0 {
					log.InfoContext(ctx, "huge pages not configured - GPU performance may be reduced",
						"hint", "run: echo 1024 > /proc/sys/vm/nr_hugepages",
					)
				}
			}
		}
	}
}

// reconcileStats tracks reconciliation metrics
type reconcileStats struct {
	orphanedCleared int
	resetAttempted  int
	resetSucceeded  int
	resetFailed     int
	mismatches      int
	suspiciousVMM   int
	errors          int
}

// isInstanceOrphaned checks if an instance should be considered orphaned
// (device attachment should be cleared).
func (m *manager) isInstanceOrphaned(ctx context.Context, instanceID string) bool {
	// If we have a liveness checker, use it for more accurate detection
	if m.livenessChecker != nil {
		// Instance is orphaned if it's not running (stopped, standby, unknown, or missing)
		return !m.livenessChecker.IsInstanceRunning(ctx, instanceID)
	}

	// Fallback: just check if instance directory exists
	instanceDir := m.paths.InstanceDir(instanceID)
	_, err := os.Stat(instanceDir)
	return os.IsNotExist(err)
}

// resetOrphanedDevice performs GPU-reset-lite for an orphaned device.
// This is safe because we've already confirmed the device is orphaned.
// Steps mirror gpu-reset.sh but are per-device and non-destructive.
func (m *manager) resetOrphanedDevice(ctx context.Context, device *Device, stats *reconcileStats) {
	log := logger.FromContext(ctx)
	stats.resetAttempted++

	log.InfoContext(ctx, "running GPU-reset-lite for orphaned device",
		"device_id", device.Id,
		"device_name", device.Name,
		"pci_address", device.PCIAddress,
		"bound_to_vfio", device.BoundToVFIO,
	)

	// Step 1: If bound to VFIO, unbind
	if device.BoundToVFIO {
		log.DebugContext(ctx, "unbinding orphaned device from VFIO", "pci_address", device.PCIAddress)
		if err := m.vfioBinder.unbindFromDriver(device.PCIAddress, "vfio-pci"); err != nil {
			log.WarnContext(ctx, "failed to unbind device from VFIO during reset",
				"device_id", device.Id,
				"pci_address", device.PCIAddress,
				"error", err,
			)
			// Continue with other steps
		}
	}

	// Step 2: Clear driver_override
	log.DebugContext(ctx, "clearing driver_override", "pci_address", device.PCIAddress)
	if err := m.vfioBinder.setDriverOverride(device.PCIAddress, ""); err != nil {
		log.WarnContext(ctx, "failed to clear driver_override during reset",
			"device_id", device.Id,
			"pci_address", device.PCIAddress,
			"error", err,
		)
		// Continue with other steps
	}

	// Step 3: Trigger driver probe to rebind to original driver
	log.DebugContext(ctx, "triggering driver probe", "pci_address", device.PCIAddress)
	if err := m.vfioBinder.triggerDriverProbe(device.PCIAddress); err != nil {
		log.WarnContext(ctx, "failed to trigger driver probe during reset",
			"device_id", device.Id,
			"pci_address", device.PCIAddress,
			"error", err,
		)
	}

	// Step 4: For NVIDIA devices, restart nvidia-persistenced
	if device.VendorID == "10de" {
		log.DebugContext(ctx, "restarting nvidia-persistenced", "pci_address", device.PCIAddress)
		if err := m.vfioBinder.startNvidiaPersistenced(); err != nil {
			log.WarnContext(ctx, "failed to restart nvidia-persistenced during reset",
				"device_id", device.Id,
				"error", err,
			)
		}
	}

	// Verify the device is now unbound from VFIO
	stillBoundToVFIO := m.vfioBinder.IsDeviceBoundToVFIO(device.PCIAddress)
	if stillBoundToVFIO {
		log.WarnContext(ctx, "device still bound to VFIO after reset-lite",
			"device_id", device.Id,
			"pci_address", device.PCIAddress,
		)
		stats.resetFailed++
	} else {
		log.InfoContext(ctx, "GPU-reset-lite completed for orphaned device",
			"device_id", device.Id,
			"device_name", device.Name,
			"pci_address", device.PCIAddress,
		)
		stats.resetSucceeded++
	}

	// Update device metadata to reflect new VFIO state
	device.BoundToVFIO = stillBoundToVFIO
	if err := m.saveDevice(device); err != nil {
		log.WarnContext(ctx, "failed to save device after reset-lite",
			"device_id", device.Id,
			"error", err,
		)
	}
}

// Helper methods

func (m *manager) loadDevice(id string) (*Device, error) {
	data, err := os.ReadFile(m.paths.DeviceMetadata(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	var device Device
	if err := json.Unmarshal(data, &device); err != nil {
		return nil, err
	}

	return &device, nil
}

func (m *manager) saveDevice(device *Device) error {
	data, err := json.MarshalIndent(device, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(m.paths.DeviceMetadata(device.Id), data, 0644)
}

func (m *manager) findByName(name string) (*Device, error) {
	entries, err := os.ReadDir(m.paths.DevicesDir())
	if err != nil {
		return nil, ErrNotFound
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		device, err := m.loadDevice(entry.Name())
		if err != nil {
			continue
		}

		if device.Name == name {
			return device, nil
		}
	}

	return nil, ErrNotFound
}

func (m *manager) findByPCIAddress(pciAddress string) (*Device, error) {
	entries, err := os.ReadDir(m.paths.DevicesDir())
	if err != nil {
		return nil, ErrNotFound
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		device, err := m.loadDevice(entry.Name())
		if err != nil {
			continue
		}

		if device.PCIAddress == pciAddress {
			return device, nil
		}
	}

	return nil, ErrNotFound
}
