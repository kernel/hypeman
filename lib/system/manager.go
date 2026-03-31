package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/kernel/hypeman/lib/paths"
)

var initrdEnsureLocks sync.Map

// Manager handles system files (kernel, initrd)
type Manager interface {
	// EnsureSystemFiles ensures all supported kernels and the current initrd exist.
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

func initrdEnsureLockKey(initrdDir string) string {
	if resolved, err := filepath.EvalSymlinks(initrdDir); err == nil {
		return resolved
	}
	return initrdDir
}

func getInitrdEnsureLock(initrdDir string) *sync.Mutex {
	key := initrdEnsureLockKey(initrdDir)
	lock, _ := initrdEnsureLocks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// EnsureSystemFiles ensures all supported kernels and the current initrd exist,
// downloading/building them if needed.
func (m *manager) EnsureSystemFiles(ctx context.Context) error {
	for _, kernelVer := range SupportedKernelVersions {
		if _, err := m.ensureKernel(kernelVer); err != nil {
			return fmt.Errorf("ensure kernel %s: %w", kernelVer, err)
		}
	}

	// Ensure initrd exists (builds if missing or stale)
	initrdLock := getInitrdEnsureLock(m.paths.SystemInitrdDir(GetArch()))
	initrdLock.Lock()
	defer initrdLock.Unlock()

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
