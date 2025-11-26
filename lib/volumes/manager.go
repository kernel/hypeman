package volumes

import (
	"context"
	"sync"
	"time"

	"github.com/nrednav/cuid2"
	"github.com/onkernel/hypeman/lib/paths"
)

// Manager provides volume lifecycle operations
type Manager interface {
	ListVolumes(ctx context.Context) ([]Volume, error)
	CreateVolume(ctx context.Context, req CreateVolumeRequest) (*Volume, error)
	GetVolume(ctx context.Context, id string) (*Volume, error)
	DeleteVolume(ctx context.Context, id string) error

	// Attachment operations (called by instance manager)
	AttachVolume(ctx context.Context, id string, req AttachVolumeRequest) error
	DetachVolume(ctx context.Context, id string) error

	// GetVolumePath returns the path to the volume data file
	GetVolumePath(id string) string
}

type manager struct {
	paths       *paths.Paths
	volumeLocks sync.Map // map[string]*sync.RWMutex - per-volume locks
}

// NewManager creates a new volumes manager
func NewManager(p *paths.Paths) Manager {
	return &manager{
		paths:       p,
		volumeLocks: sync.Map{},
	}
}

// getVolumeLock returns or creates a lock for a specific volume
func (m *manager) getVolumeLock(id string) *sync.RWMutex {
	lock, _ := m.volumeLocks.LoadOrStore(id, &sync.RWMutex{})
	return lock.(*sync.RWMutex)
}

// ListVolumes returns all volumes
func (m *manager) ListVolumes(ctx context.Context) ([]Volume, error) {
	ids, err := listVolumeIDs(m.paths)
	if err != nil {
		return nil, err
	}

	volumes := make([]Volume, 0, len(ids))
	for _, id := range ids {
		vol, err := m.GetVolume(ctx, id)
		if err != nil {
			// Skip volumes that can't be loaded
			continue
		}
		volumes = append(volumes, *vol)
	}

	return volumes, nil
}

// CreateVolume creates a new volume
func (m *manager) CreateVolume(ctx context.Context, req CreateVolumeRequest) (*Volume, error) {
	// Generate or use provided ID
	id := cuid2.Generate()
	if req.Id != nil && *req.Id != "" {
		id = *req.Id
	}

	// Check volume doesn't already exist
	if _, err := loadMetadata(m.paths, id); err == nil {
		return nil, ErrAlreadyExists
	}

	// Create volume directory
	if err := ensureVolumeDir(m.paths, id); err != nil {
		return nil, err
	}

	// Create and format the disk
	if err := createVolumeDisk(m.paths, id, req.SizeGb); err != nil {
		// Cleanup on error
		deleteVolumeData(m.paths, id)
		return nil, err
	}

	// Create metadata
	now := time.Now()
	meta := &storedMetadata{
		Id:        id,
		Name:      req.Name,
		SizeGb:    req.SizeGb,
		CreatedAt: now.Format(time.RFC3339),
	}

	// Save metadata
	if err := saveMetadata(m.paths, meta); err != nil {
		// Cleanup on error
		deleteVolumeData(m.paths, id)
		return nil, err
	}

	return m.metadataToVolume(meta), nil
}

// GetVolume returns a volume by ID
func (m *manager) GetVolume(ctx context.Context, id string) (*Volume, error) {
	lock := m.getVolumeLock(id)
	lock.RLock()
	defer lock.RUnlock()

	meta, err := loadMetadata(m.paths, id)
	if err != nil {
		return nil, err
	}

	return m.metadataToVolume(meta), nil
}

// DeleteVolume deletes a volume
func (m *manager) DeleteVolume(ctx context.Context, id string) error {
	lock := m.getVolumeLock(id)
	lock.Lock()
	defer lock.Unlock()

	// Load metadata to check attachment
	meta, err := loadMetadata(m.paths, id)
	if err != nil {
		return err
	}

	// Check if volume is attached
	if meta.AttachedTo != nil {
		return ErrInUse
	}

	// Delete volume data
	if err := deleteVolumeData(m.paths, id); err != nil {
		return err
	}

	// Clean up lock
	m.volumeLocks.Delete(id)

	return nil
}

// AttachVolume marks a volume as attached to an instance
func (m *manager) AttachVolume(ctx context.Context, id string, req AttachVolumeRequest) error {
	lock := m.getVolumeLock(id)
	lock.Lock()
	defer lock.Unlock()

	meta, err := loadMetadata(m.paths, id)
	if err != nil {
		return err
	}

	// Check if already attached
	if meta.AttachedTo != nil {
		return ErrInUse
	}

	// Update attachment info
	meta.AttachedTo = &req.InstanceID
	meta.MountPath = &req.MountPath
	meta.Readonly = req.Readonly

	return saveMetadata(m.paths, meta)
}

// DetachVolume marks a volume as detached from an instance
func (m *manager) DetachVolume(ctx context.Context, id string) error {
	lock := m.getVolumeLock(id)
	lock.Lock()
	defer lock.Unlock()

	meta, err := loadMetadata(m.paths, id)
	if err != nil {
		return err
	}

	// Clear attachment info
	meta.AttachedTo = nil
	meta.MountPath = nil
	meta.Readonly = false

	return saveMetadata(m.paths, meta)
}

// GetVolumePath returns the path to the volume data file
func (m *manager) GetVolumePath(id string) string {
	return m.paths.VolumeData(id)
}

// metadataToVolume converts stored metadata to a Volume struct
func (m *manager) metadataToVolume(meta *storedMetadata) *Volume {
	createdAt, _ := time.Parse(time.RFC3339, meta.CreatedAt)

	return &Volume{
		Id:         meta.Id,
		Name:       meta.Name,
		SizeGb:     meta.SizeGb,
		CreatedAt:  createdAt,
		AttachedTo: meta.AttachedTo,
		MountPath:  meta.MountPath,
		Readonly:   meta.Readonly,
	}
}
