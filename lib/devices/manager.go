package devices

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nrednav/cuid2"
	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/paths"
)

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
}

type manager struct {
	paths      *paths.Paths
	vfioBinder *VFIOBinder
	mu         sync.RWMutex
}

// NewManager creates a new device manager
func NewManager(p *paths.Paths) Manager {
	return &manager{
		paths:      p,
		vfioBinder: NewVFIOBinder(),
	}
}

func (m *manager) ListDevices(ctx context.Context) ([]Device, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	devicesDir := m.devicesDir()
	entries, err := os.ReadDir(devicesDir)
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

	// Validate name
	if !ValidateDeviceName(req.Name) {
		return nil, ErrInvalidName
	}

	// Validate PCI address format
	if !ValidatePCIAddress(req.PCIAddress) {
		return nil, ErrInvalidPCIAddress
	}

	// Get device info from sysfs
	deviceInfo, err := GetDeviceInfo(req.PCIAddress)
	if err != nil {
		return nil, fmt.Errorf("get device info: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if name already exists
	if _, err := m.findByName(req.Name); err == nil {
		return nil, ErrNameExists
	}

	// Check if PCI address already registered
	if _, err := m.findByPCIAddress(req.PCIAddress); err == nil {
		return nil, ErrAlreadyExists
	}

	// Generate ID
	id := cuid2.Generate()

	// Create device
	device := &Device{
		Id:          id,
		Name:        req.Name,
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
	if err := os.MkdirAll(m.deviceDir(id), 0755); err != nil {
		return nil, fmt.Errorf("create device dir: %w", err)
	}

	// Save device metadata
	if err := m.saveDevice(device); err != nil {
		os.RemoveAll(m.deviceDir(id))
		return nil, fmt.Errorf("save device: %w", err)
	}

	log.InfoContext(ctx, "registered device",
		"id", id,
		"name", req.Name,
		"pci_address", req.PCIAddress,
		"type", device.Type,
	)

	return device, nil
}

func (m *manager) GetDevice(ctx context.Context, idOrName string) (*Device, error) {
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
	if err := os.RemoveAll(m.deviceDir(id)); err != nil {
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

// Helper methods

func (m *manager) devicesDir() string {
	return filepath.Join(m.paths.GuestsDir(), "..", "devices")
}

func (m *manager) deviceDir(id string) string {
	return filepath.Join(m.devicesDir(), id)
}

func (m *manager) deviceMetadataPath(id string) string {
	return filepath.Join(m.deviceDir(id), "metadata.json")
}

func (m *manager) loadDevice(id string) (*Device, error) {
	data, err := os.ReadFile(m.deviceMetadataPath(id))
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

	return os.WriteFile(m.deviceMetadataPath(device.Id), data, 0644)
}

func (m *manager) findByName(name string) (*Device, error) {
	entries, err := os.ReadDir(m.devicesDir())
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
	entries, err := os.ReadDir(m.devicesDir())
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


