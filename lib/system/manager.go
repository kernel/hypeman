package system

import (
	"context"
	"fmt"
	"os"

	"github.com/kernel/hypeman/lib/paths"
)

// Manager handles system files (kernel, initrd)
type Manager interface {
	// EnsureSystemFiles ensures default kernel and initrd exist
	EnsureSystemFiles(ctx context.Context) error

	// GetKernelPath returns path to kernel file
	GetKernelPath(version KernelVersion) (string, error)

	// GetInitrdPath returns path to current initrd file
	GetInitrdPath() (string, error)

	// GetDefaultKernelVersion returns the default kernel version
	GetDefaultKernelVersion() KernelVersion
}

type manager struct {
	paths *paths.Paths
}

// NewManager creates a new system manager
func NewManager(p *paths.Paths) Manager {
	return &manager{
		paths: p,
	}
}

// EnsureSystemFiles ensures default kernel and initrd exist, downloading/building if needed
func (m *manager) EnsureSystemFiles(ctx context.Context) error {
	kernelVer := m.GetDefaultKernelVersion()

	// Ensure kernel exists
	if _, err := m.ensureKernel(kernelVer); err != nil {
		return fmt.Errorf("ensure kernel %s: %w", kernelVer, err)
	}

	// Ensure initrd exists (builds if missing or stale)
	if _, err := m.ensureInitrd(ctx); err != nil {
		return fmt.Errorf("ensure initrd: %w", err)
	}

	return nil
}

// GetKernelPath returns the path to a kernel version
func (m *manager) GetKernelPath(version KernelVersion) (string, error) {
	arch := GetArch()
	path := m.paths.SystemKernel(string(version), arch)
	return path, nil
}

// GetInitrdPath returns the path to the current initrd file
func (m *manager) GetInitrdPath() (string, error) {
	arch := GetArch()
	latestLink := m.paths.SystemInitrdLatest(arch)
	
	// Read the symlink to get the timestamp
	target, err := os.Readlink(latestLink)
	if err != nil {
		return "", fmt.Errorf("read latest symlink: %w", err)
	}
	
	return m.paths.SystemInitrdTimestamp(target, arch), nil
}

// GetDefaultKernelVersion returns the default kernel version
func (m *manager) GetDefaultKernelVersion() KernelVersion {
	return DefaultKernelVersion
}

