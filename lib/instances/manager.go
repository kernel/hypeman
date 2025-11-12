package instances

import (
	"context"
	"fmt"
	"sync"

	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/system"
)

type Manager interface {
	ListInstances(ctx context.Context) ([]Instance, error)
	CreateInstance(ctx context.Context, req CreateInstanceRequest) (*Instance, error)
	GetInstance(ctx context.Context, id string) (*Instance, error)
	DeleteInstance(ctx context.Context, id string) error
	StandbyInstance(ctx context.Context, id string) (*Instance, error)
	RestoreInstance(ctx context.Context, id string) (*Instance, error)
	GetInstanceLogs(ctx context.Context, id string, follow bool, tail int) (string, error)
	AttachVolume(ctx context.Context, id string, volumeId string, req AttachVolumeRequest) (*Instance, error)
	DetachVolume(ctx context.Context, id string, volumeId string) (*Instance, error)
}

type manager struct {
	paths          *paths.Paths
	imageManager   images.Manager
	systemManager  system.Manager
	maxOverlaySize int64    // Maximum overlay disk size in bytes
	instanceLocks  sync.Map // map[string]*sync.RWMutex - per-instance locks
	hostTopology   *HostTopology // Cached host CPU topology
}

// NewManager creates a new instances manager
func NewManager(p *paths.Paths, imageManager images.Manager, systemManager system.Manager, maxOverlaySize int64) Manager {
	return &manager{
		paths:          p,
		imageManager:   imageManager,
		systemManager:  systemManager,
		maxOverlaySize: maxOverlaySize,
		instanceLocks:  sync.Map{},
		hostTopology:   detectHostTopology(), // Detect and cache host topology
	}
}

// getInstanceLock returns or creates a lock for a specific instance
func (m *manager) getInstanceLock(id string) *sync.RWMutex {
	lock, _ := m.instanceLocks.LoadOrStore(id, &sync.RWMutex{})
	return lock.(*sync.RWMutex)
}

// CreateInstance creates and starts a new instance
func (m *manager) CreateInstance(ctx context.Context, req CreateInstanceRequest) (*Instance, error) {
	// Note: ID is generated inside createInstance, so we can't lock before calling it.
	// This is safe because:
	// 1. ULID generation is unique
	// 2. Filesystem mkdir is atomic per instance directory
	// 3. Concurrent creates of different instances don't conflict
	return m.createInstance(ctx, req)
}

// DeleteInstance stops and deletes an instance
func (m *manager) DeleteInstance(ctx context.Context, id string) error {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	
	err := m.deleteInstance(ctx, id)
	if err == nil {
		// Clean up the lock after successful deletion
		m.instanceLocks.Delete(id)
	}
	return err
}

// StandbyInstance puts an instance in standby (pause, snapshot, delete VMM)
func (m *manager) StandbyInstance(ctx context.Context, id string) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	return m.standbyInstance(ctx, id)
}

// RestoreInstance restores an instance from standby
func (m *manager) RestoreInstance(ctx context.Context, id string) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	return m.restoreInstance(ctx, id)
}

// ListInstances returns all instances
func (m *manager) ListInstances(ctx context.Context) ([]Instance, error) {
	// No lock - eventual consistency is acceptable for list operations.
	// State is derived dynamically, so list is always reasonably current.
	return m.listInstances(ctx)
}

// GetInstance returns a single instance
func (m *manager) GetInstance(ctx context.Context, id string) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.RLock()
	defer lock.RUnlock()
	return m.getInstance(ctx, id)
}

// GetInstanceLogs returns instance console logs
func (m *manager) GetInstanceLogs(ctx context.Context, id string, follow bool, tail int) (string, error) {
	lock := m.getInstanceLock(id)
	lock.RLock()
	defer lock.RUnlock()
	return m.getInstanceLogs(ctx, id, follow, tail)
}

// AttachVolume attaches a volume to an instance (not yet implemented)
func (m *manager) AttachVolume(ctx context.Context, id string, volumeId string, req AttachVolumeRequest) (*Instance, error) {
	return nil, fmt.Errorf("attach volume not yet implemented")
}

// DetachVolume detaches a volume from an instance (not yet implemented)
func (m *manager) DetachVolume(ctx context.Context, id string, volumeId string) (*Instance, error) {
	return nil, fmt.Errorf("detach volume not yet implemented")
}
