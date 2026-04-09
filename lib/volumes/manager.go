package volumes

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/tags"
	"github.com/nrednav/cuid2"
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
	// Multi-attach rules (dynamic based on current state):
	// - If no attachments: allow any mode (rw or ro)
	// - If existing attachments are ro: only allow new ro attachments
	// - Multiple rw attachments (ReadWriteMany): internally backed by NFS,
	//   transparent to the caller. NFS is set up automatically when a second
	//   rw attachment is requested.
	AttachVolume(ctx context.Context, id string, req AttachVolumeRequest) error
	DetachVolume(ctx context.Context, volumeID string, instanceID string) error

	// GetVolumePath returns the path to the volume data file
	GetVolumePath(id string) string

	// GetVolumeNFSInfo returns NFS serving details if the volume is NFS-served, nil otherwise.
	// Used by the instance manager to decide whether to pass a block device or NFS mount info.
	GetVolumeNFSInfo(ctx context.Context, id string) (*NFSInfo, error)

	// TotalVolumeBytes returns the total size of all volumes.
	// Used by the resource manager for disk capacity tracking.
	TotalVolumeBytes(ctx context.Context) (int64, error)
}

type manager struct {
	paths                 *paths.Paths
	maxTotalVolumeStorage int64    // Maximum total volume storage in bytes (0 = unlimited)
	volumeLocks           sync.Map // map[string]*sync.RWMutex - per-volume locks
	totalsMu              sync.RWMutex
	totalVolumeBytes      int64
	totalVolumeBytesReady bool
	metrics               *Metrics
	nfs                   *nfsManager
	nfsHost               string // Host IP for NFS mounts (VM bridge gateway)
}

// NewManager creates a new volumes manager.
// maxTotalVolumeStorage is the maximum total volume storage in bytes (0 = unlimited).
// nfsHost is the host IP that VMs use to reach the NFS server (typically the bridge gateway).
// If meter is nil, metrics are disabled.
func NewManager(p *paths.Paths, maxTotalVolumeStorage int64, meter metric.Meter) Manager {
	m := &manager{
		paths:                 p,
		maxTotalVolumeStorage: maxTotalVolumeStorage,
		volumeLocks:           sync.Map{},
		nfs:                   newNFSManager(p),
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

// NFSHostSetter allows setting the NFS host IP after initialization.
type NFSHostSetter interface {
	SetNFSHost(host string)
}

// SetNFSHost sets the host IP used for NFS mounts. Called after network initialization
// when the bridge gateway IP is known.
func (m *manager) SetNFSHost(host string) {
	m.nfsHost = host
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

func (m *manager) getTotalVolumeBytes(ctx context.Context) (int64, error) {
	m.totalsMu.RLock()
	if m.totalVolumeBytesReady {
		total := m.totalVolumeBytes
		m.totalsMu.RUnlock()
		return total, nil
	}
	m.totalsMu.RUnlock()

	total, err := m.calculateTotalVolumeStorage(ctx)
	if err != nil {
		return 0, err
	}

	m.totalsMu.Lock()
	if !m.totalVolumeBytesReady {
		m.totalVolumeBytes = total
		m.totalVolumeBytesReady = true
	}
	total = m.totalVolumeBytes
	m.totalsMu.Unlock()

	return total, nil
}

func (m *manager) addVolumeBytes(sizeBytes int64) {
	m.totalsMu.Lock()
	defer m.totalsMu.Unlock()
	if !m.totalVolumeBytesReady {
		return
	}
	m.totalVolumeBytes += sizeBytes
}

func (m *manager) subtractVolumeBytes(sizeBytes int64) {
	m.totalsMu.Lock()
	defer m.totalsMu.Unlock()
	if !m.totalVolumeBytesReady {
		return
	}
	m.totalVolumeBytes -= sizeBytes
}

// CreateVolume creates a new volume
func (m *manager) CreateVolume(ctx context.Context, req CreateVolumeRequest) (*Volume, error) {
	start := time.Now()
	if err := tags.Validate(req.Tags); err != nil {
		return nil, err
	}

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
		currentStorage, err := m.getTotalVolumeBytes(ctx)
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
		Tags:      tags.Clone(req.Tags),
		CreatedAt: now.Format(time.RFC3339),
	}

	// Save metadata
	if err := saveMetadata(m.paths, meta); err != nil {
		// Cleanup on error
		deleteVolumeData(m.paths, id)
		return nil, err
	}

	m.addVolumeBytes(int64(req.SizeGb) * 1024 * 1024 * 1024)

	m.recordCreateDuration(ctx, start, "success")
	return m.metadataToVolume(meta), nil
}

// CreateVolumeFromArchive creates a new volume pre-populated with content from a tar.gz archive.
// The archive is safely extracted with size limits to prevent tar bombs.
func (m *manager) CreateVolumeFromArchive(ctx context.Context, req CreateVolumeFromArchiveRequest, archive io.Reader) (*Volume, error) {
	start := time.Now()
	if err := tags.Validate(req.Tags); err != nil {
		return nil, err
	}

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
		currentStorage, err := m.getTotalVolumeBytes(ctx)
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
		Tags:      tags.Clone(req.Tags),
		CreatedAt: now.Format(time.RFC3339),
	}

	// Save metadata
	if err := saveMetadata(m.paths, meta); err != nil {
		deleteVolumeData(m.paths, id)
		return nil, err
	}

	m.addVolumeBytes(int64(actualSizeGb) * 1024 * 1024 * 1024)

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

	m.subtractVolumeBytes(int64(meta.SizeGb) * 1024 * 1024 * 1024)

	// Clean up lock
	m.volumeLocks.Delete(id)

	return nil
}

// AttachVolume marks a volume as attached to an instance.
// Multi-attach rules (dynamic based on current state):
//   - If no attachments: allow any mode (rw or ro) via block device
//   - If existing attachments are all ro: only allow new ro attachments
//   - If existing attachment is rw (block device) and new is rw: enable NFS
//     for ReadWriteMany. The volume is loop-mounted on the host and exported
//     via NFS. The new attachment (and any subsequent ones) use NFS.
//   - If volume is already NFS-served: additional rw attachments use NFS
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

	useNFS := false

	if len(meta.Attachments) > 0 {
		hasRW := false
		allRO := true
		for _, att := range meta.Attachments {
			if !att.Readonly {
				hasRW = true
				allRO = false
			}
		}

		if allRO && !req.Readonly {
			// Existing attachments are all ro, new is rw → conflict
			return fmt.Errorf("cannot attach read-write: volume has existing read-only attachments")
		}

		if hasRW && req.Readonly {
			// Existing has rw, new is ro → conflict (rw is exclusive or NFS-only)
			return fmt.Errorf("cannot attach read-only: volume has existing read-write attachment")
		}

		if hasRW && !req.Readonly {
			// ReadWriteMany scenario: both existing and new want rw.
			// Transparently enable NFS serving.
			if m.nfsHost == "" {
				return fmt.Errorf("cannot attach read-write to multiple instances: NFS host not configured (networking required)")
			}

			// Start NFS serving if not already active
			if meta.NFS == nil {
				exportPath, err := m.nfs.startServing(id)
				if err != nil {
					return fmt.Errorf("start nfs serving for ReadWriteMany: %w", err)
				}
				meta.NFS = &storedNFSInfo{
					Host:       m.nfsHost,
					ExportPath: exportPath,
				}
			}
			useNFS = true
		}
	}

	// If volume is already NFS-served, new rw attachments use NFS
	if meta.NFS != nil && !req.Readonly {
		useNFS = true
	}

	// Add new attachment
	meta.Attachments = append(meta.Attachments, storedAttachment{
		InstanceID: req.InstanceID,
		MountPath:  req.MountPath,
		Readonly:   req.Readonly,
		NFS:        useNFS,
	})

	return saveMetadata(m.paths, meta)
}

// DetachVolume removes the attachment for a specific instance.
// When the last NFS-using attachment is removed, NFS serving is stopped.
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

	// Check if NFS serving should be stopped.
	// Stop when there are no remaining NFS-based rw attachments.
	if meta.NFS != nil {
		hasNFSAttachments := false
		for _, att := range meta.Attachments {
			if att.NFS {
				hasNFSAttachments = true
				break
			}
		}
		if !hasNFSAttachments {
			// No more NFS consumers — tear down NFS serving
			if err := m.nfs.stopServing(volumeID); err != nil {
				// Log but don't fail the detach
				fmt.Fprintf(os.Stderr, "warning: failed to stop NFS serving for volume %s: %v\n", volumeID, err)
			}
			meta.NFS = nil
		}
	}

	return saveMetadata(m.paths, meta)
}

// GetVolumePath returns the path to the volume data file
func (m *manager) GetVolumePath(id string) string {
	return m.paths.VolumeData(id)
}

// GetVolumeNFSInfo returns NFS serving details if the volume is NFS-served, nil otherwise.
func (m *manager) GetVolumeNFSInfo(ctx context.Context, id string) (*NFSInfo, error) {
	lock := m.getVolumeLock(id)
	lock.RLock()
	defer lock.RUnlock()

	meta, err := loadMetadata(m.paths, id)
	if err != nil {
		return nil, err
	}
	if meta.NFS == nil {
		return nil, nil
	}
	return &NFSInfo{
		Host:       meta.NFS.Host,
		ExportPath: meta.NFS.ExportPath,
	}, nil
}

// TotalVolumeBytes returns the total size of all volumes.
func (m *manager) TotalVolumeBytes(ctx context.Context) (int64, error) {
	return m.getTotalVolumeBytes(ctx)
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
			NFS:        att.NFS,
		}
	}

	vol := &Volume{
		Id:          meta.Id,
		Name:        meta.Name,
		SizeGb:      meta.SizeGb,
		Tags:        tags.Clone(meta.Tags),
		CreatedAt:   createdAt,
		Attachments: attachments,
	}

	if meta.NFS != nil {
		vol.NFS = &NFSInfo{
			Host:       meta.NFS.Host,
			ExportPath: meta.NFS.ExportPath,
		}
	}

	return vol
}
