package system

import (
	"context"
	"fmt"
	"path/filepath"
)

// Manager handles system files (kernel, initrd)
type Manager interface {
	// EnsureSystemFiles ensures default kernel and initrd exist
	EnsureSystemFiles(ctx context.Context) error

	// GetKernelPath returns path to kernel file
	GetKernelPath(version KernelVersion) (string, error)

	// GetInitrdPath returns path to initrd file
	GetInitrdPath(version InitrdVersion) (string, error)

	// GetDefaultVersions returns the default kernel and initrd versions
	GetDefaultVersions() (KernelVersion, InitrdVersion)
}

type manager struct {
	dataDir string
}

// NewManager creates a new system manager
func NewManager(dataDir string) Manager {
	return &manager{
		dataDir: dataDir,
	}
}

// EnsureSystemFiles ensures default kernel and initrd exist, downloading/building if needed
func (m *manager) EnsureSystemFiles(ctx context.Context) error {
	kernelVer, initrdVer := m.GetDefaultVersions()

	// Ensure kernel exists
	if _, err := m.ensureKernel(kernelVer); err != nil {
		return fmt.Errorf("ensure kernel %s: %w", kernelVer, err)
	}

	// Ensure initrd exists
	if _, err := m.ensureInitrd(ctx, initrdVer); err != nil {
		return fmt.Errorf("ensure initrd %s: %w", initrdVer, err)
	}

	return nil
}

// GetKernelPath returns the path to a kernel version
func (m *manager) GetKernelPath(version KernelVersion) (string, error) {
	arch := GetArch()
	path := filepath.Join(m.dataDir, "system", "kernel", string(version), arch, "vmlinux")
	return path, nil
}

// GetInitrdPath returns the path to an initrd version
func (m *manager) GetInitrdPath(version InitrdVersion) (string, error) {
	arch := GetArch()
	path := filepath.Join(m.dataDir, "system", "initrd", string(version), arch, "initrd")
	return path, nil
}

// GetDefaultVersions returns the default kernel and initrd versions
func (m *manager) GetDefaultVersions() (KernelVersion, InitrdVersion) {
	return DefaultKernelVersion, DefaultInitrdVersion
}

