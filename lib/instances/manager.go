package instances

import (
	"context"
	"fmt"
	"sync"

	"github.com/onkernel/hypeman/lib/images"
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
	dataDir        string
	imageManager   images.Manager
	systemManager  system.Manager
	maxOverlaySize int64        // Maximum overlay disk size in bytes
	mu             sync.RWMutex // Protects concurrent access to instances
}

// NewManager creates a new instances manager
func NewManager(dataDir string, imageManager images.Manager, systemManager system.Manager, maxOverlaySize int64) Manager {
	return &manager{
		dataDir:        dataDir,
		imageManager:   imageManager,
		systemManager:  systemManager,
		maxOverlaySize: maxOverlaySize,
	}
}

// CreateInstance creates and starts a new instance
func (m *manager) CreateInstance(ctx context.Context, req CreateInstanceRequest) (*Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createInstance(ctx, req)
}

// DeleteInstance stops and deletes an instance
func (m *manager) DeleteInstance(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleteInstance(ctx, id)
}

// StandbyInstance puts an instance in standby (pause, snapshot, delete VMM)
func (m *manager) StandbyInstance(ctx context.Context, id string) (*Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.standbyInstance(ctx, id)
}

// RestoreInstance restores an instance from standby
func (m *manager) RestoreInstance(ctx context.Context, id string) (*Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restoreInstance(ctx, id)
}

// ListInstances returns all instances
func (m *manager) ListInstances(ctx context.Context) ([]Instance, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.listInstances(ctx)
}

// GetInstance returns a single instance
func (m *manager) GetInstance(ctx context.Context, id string) (*Instance, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.getInstance(ctx, id)
}

// GetInstanceLogs returns instance console logs
func (m *manager) GetInstanceLogs(ctx context.Context, id string, follow bool, tail int) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
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
