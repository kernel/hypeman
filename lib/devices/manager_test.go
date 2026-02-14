package devices

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateDeviceName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"valid alphanumeric", "l4gpu", true},
		{"valid with underscore", "my_gpu", true},
		{"valid with dash", "gpu-1", true},
		{"valid with dot", "nvidia.l4", true},
		{"valid mixed", "my-gpu_01.test", true},
		{"valid starting with number", "1gpu", true},
		{"invalid empty", "", false},
		{"invalid single char", "a", false}, // pattern requires at least 2 chars
		{"invalid starts with dash", "-gpu", false},
		{"invalid starts with underscore", "_gpu", false},
		{"invalid starts with dot", ".gpu", false},
		{"invalid contains space", "my gpu", false},
		{"invalid contains special char", "gpu@1", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ValidateDeviceName(tt.input)
			assert.Equal(t, tt.expected, result, "ValidateDeviceName(%q)", tt.input)
		})
	}
}

func TestValidatePCIAddress(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"valid standard", "0000:00:00.0", true},
		{"valid with letters", "0000:a2:00.0", true},
		{"valid uppercase", "0000:A2:00.0", true},
		{"valid mixed case", "0000:aB:c1.2", true},
		{"invalid too short", "0000:00:0.0", false},
		{"invalid no domain", "00:00.0", false},
		{"invalid missing colon", "000000:00.0", false},
		{"invalid missing dot", "0000:00:000", false},
		{"invalid extra segment", "0000:00:00:00.0", false},
		{"invalid empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ValidatePCIAddress(tt.input)
			assert.Equal(t, tt.expected, result, "ValidatePCIAddress(%q)", tt.input)
		})
	}
}

func TestDetermineDeviceType(t *testing.T) {
	// This test is limited since it reads from sysfs
	// We test the function structure but can't mock sysfs easily
	t.Run("returns generic for nil device", func(t *testing.T) {
		device := &AvailableDevice{
			PCIAddress: "0000:99:99.0", // Non-existent device
		}
		deviceType := DetermineDeviceType(device)
		assert.Equal(t, DeviceTypeGeneric, deviceType)
	})
}

func TestGetDeviceSysfsPath(t *testing.T) {
	tests := []struct {
		pciAddress string
		expected   string
	}{
		{"0000:a2:00.0", "/sys/bus/pci/devices/0000:a2:00.0/"},
		{"0000:00:1f.0", "/sys/bus/pci/devices/0000:00:1f.0/"},
	}

	for _, tt := range tests {
		t.Run(tt.pciAddress, func(t *testing.T) {
			result := GetDeviceSysfsPath(tt.pciAddress)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetVendorName(t *testing.T) {
	tests := []struct {
		vendorID string
		expected string
	}{
		{"10de", "NVIDIA Corporation"},
		{"1002", "AMD/ATI"},
		{"8086", "Intel Corporation"},
		{"1234", "Unknown Vendor"},
	}

	for _, tt := range tests {
		t.Run(tt.vendorID, func(t *testing.T) {
			result := getVendorName(tt.vendorID)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetDeviceName(t *testing.T) {
	tests := []struct {
		name      string
		vendorID  string
		deviceID  string
		classCode string
		expected  string
	}{
		{"NVIDIA L4", "10de", "27b8", "0x030200", "L4"},
		{"NVIDIA RTX 4090", "10de", "2684", "0x030000", "RTX 4090"},
		{"Unknown NVIDIA", "10de", "9999", "0x030000", "VGA Controller"},
		{"Generic VGA", "1234", "5678", "0x030000", "VGA Controller"},
		{"Generic 3D", "1234", "5678", "0x030200", "3D Controller"},
		{"Audio device", "1234", "5678", "0x040300", "Audio Device"},
		{"Unknown class", "1234", "5678", "0x999999", "PCI Device"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getDeviceName(tt.vendorID, tt.deviceID, tt.classCode)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestVFIOBinderIsVFIOAvailable(t *testing.T) {
	binder := NewVFIOBinder()
	// Just test that it doesn't panic
	_ = binder.IsVFIOAvailable()
}

func TestDeviceTypes(t *testing.T) {
	t.Run("device type constants", func(t *testing.T) {
		require.Equal(t, DeviceType("gpu"), DeviceTypeGPU)
		require.Equal(t, DeviceType("pci"), DeviceTypeGeneric)
	})
}

func TestErrors(t *testing.T) {
	t.Run("error types are distinct", func(t *testing.T) {
		assert.NotEqual(t, ErrNotFound, ErrInUse)
		assert.NotEqual(t, ErrNotBound, ErrAlreadyBound)
		assert.NotEqual(t, ErrAlreadyExists, ErrNameExists)
	})

	t.Run("error messages are meaningful", func(t *testing.T) {
		assert.Contains(t, ErrNotFound.Error(), "not found")
		assert.Contains(t, ErrInUse.Error(), "in use")
		assert.Contains(t, ErrInvalidName.Error(), "pattern")
	})
}


