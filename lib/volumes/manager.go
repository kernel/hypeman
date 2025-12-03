package volumes

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/nrednav/cuid2"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/paths"
	"go.opentelemetry.io/otel/metric"
)

// Manager provides volume lifecycle operations
type Manager interface {
	ListVolumes(ctx context.Context) ([]Volume, error)
	CreateVolume(ctx context.Context, req CreateVolumeRequest) (*Volume, error)
	CreateVolumeFromArchive(ctx context.Context, req CreateVolumeFromArchiveRequest, archive io.Reader) (*Volume, error)
	GetVolume(ctx context.Context, id string) (*Volume, error)
	GetVolumeByName(ctx context.Context, name string) (*Volume, error)
	DeleteVolume(ctx context.Context, id string) error

	// Attachment operations (called by instance manager)
	// Multi-attach rules:
	// - If no attachments: allow any mode (rw or ro)
	// - If existing attachment is rw: reject all new attachments
	// - If existing attachments are ro: only allow new ro attachments
	AttachVolume(ctx context.Context, id string, req AttachVolumeRequest) error
	DetachVolume(ctx context.Context, volumeID string, instanceID string) error

	// GetVolumePath returns the path to the volume data file
	GetVolumePath(id string) string
}

type manager struct {
	paths                 *paths.Paths
	maxTotalVolumeStorage int64    // Maximum total volume storage in bytes (0 = unlimited)
	volumeLocks           sync.Map // map[string]*sync.RWMutex - per-volume locks
	metrics               *Metrics
}

// NewManager creates a new volumes manager.
// maxTotalVolumeStorage is the maximum total volume storage in bytes (0 = unlimited).
// If meter is nil, metrics are disabled.
func NewManager(p *paths.Paths, maxTotalVolumeStorage int64, meter metric.Meter) Manager {
	m := &manager{
		paths:                 p,
		maxTotalVolumeStorage: maxTotalVolumeStorage,
		volumeLocks:           sync.Map{},
	}

	// Initialize metrics if meter is provided
	if meter != nil {
		metrics, err := newVolumeMetrics(meter, m)
		if err == nil {
			m.metrics = metrics
		}
	}

	return m
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

// calculateTotalVolumeStorage calculates total storage used by all volumes
func (m *manager) calculateTotalVolumeStorage(ctx context.Context) (int64, error) {
	volumes, err := m.ListVolumes(ctx)
	if err != nil {
		return 0, err
	}

	var totalBytes int64
	for _, vol := range volumes {
		totalBytes += int64(vol.SizeGb) * 1024 * 1024 * 1024
	}
	return totalBytes, nil
}

// CreateVolume creates a new volume
func (m *manager) CreateVolume(ctx context.Context, req CreateVolumeRequest) (*Volume, error) {
	start := time.Now()

	// Generate or use provided ID
	id := cuid2.Generate()
	if req.Id != nil && *req.Id != "" {
		id = *req.Id
	}

	// Check volume doesn't already exist
	if _, err := loadMetadata(m.paths, id); err == nil {
		return nil, ErrAlreadyExists
	}

	// Check total volume storage limit
	if m.maxTotalVolumeStorage > 0 {
		currentStorage, err := m.calculateTotalVolumeStorage(ctx)
		if err != nil {
			// Log but don't fail - continue with creation
			// (better to allow creation than block due to listing error)
		} else {
			newVolumeSize := int64(req.SizeGb) * 1024 * 1024 * 1024
			if currentStorage+newVolumeSize > m.maxTotalVolumeStorage {
				return nil, fmt.Errorf("total volume storage would be %d bytes, exceeds limit of %d bytes", currentStorage+newVolumeSize, m.maxTotalVolumeStorage)
			}
		}
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

	m.recordCreateDuration(ctx, start, "success")
	return m.metadataToVolume(meta), nil
}

// CreateVolumeFromArchive creates a new volume pre-populated with content from a tar.gz archive.
// The archive is safely extracted with size limits to prevent tar bombs.
func (m *manager) CreateVolumeFromArchive(ctx context.Context, req CreateVolumeFromArchiveRequest, archive io.Reader) (*Volume, error) {
	start := time.Now()

	// Generate or use provided ID
	id := cuid2.Generate()
	if req.Id != nil && *req.Id != "" {
		id = *req.Id
	}

	// Check volume doesn't already exist
	if _, err := loadMetadata(m.paths, id); err == nil {
		return nil, ErrAlreadyExists
	}

	maxBytes := int64(req.SizeGb) * 1024 * 1024 * 1024

	// Check total volume storage limit
	if m.maxTotalVolumeStorage > 0 {
		currentStorage, err := m.calculateTotalVolumeStorage(ctx)
		if err != nil {
			// Log but don't fail - continue with creation
		} else {
			if currentStorage+maxBytes > m.maxTotalVolumeStorage {
				return nil, fmt.Errorf("total volume storage would be %d bytes, exceeds limit of %d bytes", currentStorage+maxBytes, m.maxTotalVolumeStorage)
			}
		}
	}

	// Create temp directory for extraction
	tempDir, err := os.MkdirTemp("", "volume-archive-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	// Extract archive with size limit
	_, err = ExtractTarGz(archive, tempDir, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("extract archive: %w", err)
	}

	// Create volume directory
	if err := ensureVolumeDir(m.paths, id); err != nil {
		return nil, err
	}

	// Create ext4 disk from extracted content
	diskPath := m.paths.VolumeData(id)
	diskSize, err := images.ExportRootfs(tempDir, diskPath, images.FormatExt4)
	if err != nil {
		deleteVolumeData(m.paths, id)
		return nil, fmt.Errorf("create disk from content: %w", err)
	}

	// Calculate actual size in GB (round up)
	actualSizeGb := int((diskSize + 1024*1024*1024 - 1) / (1024 * 1024 * 1024))
	if actualSizeGb < 1 {
		actualSizeGb = 1
	}

	// Create metadata
	now := time.Now()
	meta := &storedMetadata{
		Id:        id,
		Name:      req.Name,
		SizeGb:    actualSizeGb,
		CreatedAt: now.Format(time.RFC3339),
	}

	// Save metadata
	if err := saveMetadata(m.paths, meta); err != nil {
		deleteVolumeData(m.paths, id)
		return nil, err
	}

	m.recordCreateDuration(ctx, start, "success")
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

// GetVolumeByName returns a volume by name
// Returns ErrNotFound if no volume matches, ErrAmbiguousName if multiple match
func (m *manager) GetVolumeByName(ctx context.Context, name string) (*Volume, error) {
	volumes, err := m.ListVolumes(ctx)
	if err != nil {
		return nil, err
	}

	var matches []Volume
	for _, vol := range volumes {
		if vol.Name == name {
			matches = append(matches, vol)
		}
	}

	if len(matches) == 0 {
		return nil, ErrNotFound
	}
	if len(matches) > 1 {
		return nil, ErrAmbiguousName
	}

	return &matches[0], nil
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

	// Check if volume has any attachments
	if len(meta.Attachments) > 0 {
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
// Multi-attach rules (dynamic based on current state):
// - If no attachments: allow any mode (rw or ro)
// - If existing attachment is rw: reject all new attachments
// - If existing attachments are ro: only allow new ro attachments
func (m *manager) AttachVolume(ctx context.Context, id string, req AttachVolumeRequest) error {
	lock := m.getVolumeLock(id)
	lock.Lock()
	defer lock.Unlock()

	meta, err := loadMetadata(m.paths, id)
	if err != nil {
		return err
	}

	// Check if this instance is already attached
	for _, att := range meta.Attachments {
		if att.InstanceID == req.InstanceID {
			return fmt.Errorf("volume already attached to instance %s", req.InstanceID)
		}
	}

	// Apply multi-attach rules
	if len(meta.Attachments) > 0 {
		// Check if any existing attachment is read-write
		for _, att := range meta.Attachments {
			if !att.Readonly {
				return fmt.Errorf("volume has exclusive read-write attachment to instance %s", att.InstanceID)
			}
		}
		// Existing attachments are all read-only, new attachment must also be read-only
		if !req.Readonly {
			return fmt.Errorf("cannot attach read-write: volume has existing read-only attachments")
		}
	}

	// Add new attachment
	meta.Attachments = append(meta.Attachments, storedAttachment{
		InstanceID: req.InstanceID,
		MountPath:  req.MountPath,
		Readonly:   req.Readonly,
	})

	return saveMetadata(m.paths, meta)
}

// DetachVolume removes the attachment for a specific instance
func (m *manager) DetachVolume(ctx context.Context, volumeID string, instanceID string) error {
	lock := m.getVolumeLock(volumeID)
	lock.Lock()
	defer lock.Unlock()

	meta, err := loadMetadata(m.paths, volumeID)
	if err != nil {
		return err
	}

	// Find and remove the attachment for this instance
	found := false
	newAttachments := make([]storedAttachment, 0, len(meta.Attachments))
	for _, att := range meta.Attachments {
		if att.InstanceID == instanceID {
			found = true
			continue // Skip this attachment (remove it)
		}
		newAttachments = append(newAttachments, att)
	}

	if !found {
		return fmt.Errorf("volume not attached to instance %s", instanceID)
	}

	meta.Attachments = newAttachments
	return saveMetadata(m.paths, meta)
}

// GetVolumePath returns the path to the volume data file
func (m *manager) GetVolumePath(id string) string {
	return m.paths.VolumeData(id)
}

// metadataToVolume converts stored metadata to a Volume struct
func (m *manager) metadataToVolume(meta *storedMetadata) *Volume {
	createdAt, _ := time.Parse(time.RFC3339, meta.CreatedAt)

	// Convert stored attachments to domain attachments
	attachments := make([]Attachment, len(meta.Attachments))
	for i, att := range meta.Attachments {
		attachments[i] = Attachment{
			InstanceID: att.InstanceID,
			MountPath:  att.MountPath,
			Readonly:   att.Readonly,
		}
	}

	return &Volume{
		Id:          meta.Id,
		Name:        meta.Name,
		SizeGb:      meta.SizeGb,
		CreatedAt:   createdAt,
		Attachments: attachments,
	}
}
