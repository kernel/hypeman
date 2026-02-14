package volumes

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
)

// Filesystem structure:
// {dataDir}/volumes/{volume-id}/
//   data.raw        # ext4-formatted sparse disk
//   metadata.json   # Volume metadata

// storedAttachment represents an attachment in stored metadata
type storedAttachment struct {
	InstanceID string `json:"instance_id"`
	MountPath  string `json:"mount_path"`
	Readonly   bool   `json:"readonly"`
}

// storedMetadata represents volume metadata that is persisted to disk
type storedMetadata struct {
	Id          string             `json:"id"`
	Name        string             `json:"name"`
	SizeGb      int                `json:"size_gb"`
	CreatedAt   string             `json:"created_at"` // RFC3339 format
	Attachments []storedAttachment `json:"attachments,omitempty"`
}

// ensureVolumeDir creates the volume directory
func ensureVolumeDir(p *paths.Paths, id string) error {
	dir := p.VolumeDir(id)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create volume directory %s: %w", dir, err)
	}
	return nil
}

// loadMetadata loads volume metadata from disk
func loadMetadata(p *paths.Paths, id string) (*storedMetadata, error) {
	metaPath := p.VolumeMetadata(id)

	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read metadata: %w", err)
	}

	var meta storedMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}

	return &meta, nil
}

// saveMetadata saves volume metadata to disk
func saveMetadata(p *paths.Paths, meta *storedMetadata) error {
	metaPath := p.VolumeMetadata(meta.Id)

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	if err := os.WriteFile(metaPath, data, 0644); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}

	return nil
}

// createVolumeDisk creates a sparse disk file and formats it as ext4
func createVolumeDisk(p *paths.Paths, id string, sizeGb int) error {
	diskPath := p.VolumeData(id)
	sizeBytes := int64(sizeGb) * 1024 * 1024 * 1024
	return images.CreateEmptyExt4Disk(diskPath, sizeBytes)
}

// deleteVolumeData removes all volume data from disk
func deleteVolumeData(p *paths.Paths, id string) error {
	volDir := p.VolumeDir(id)

	if err := os.RemoveAll(volDir); err != nil {
		return fmt.Errorf("remove volume directory: %w", err)
	}

	return nil
}

// listVolumeIDs returns all volume IDs by scanning the volumes directory
func listVolumeIDs(p *paths.Paths) ([]string, error) {
	volumesDir := p.VolumesDir()

	// Ensure volumes directory exists
	if err := os.MkdirAll(volumesDir, 0755); err != nil {
		return nil, fmt.Errorf("create volumes directory: %w", err)
	}

	entries, err := os.ReadDir(volumesDir)
	if err != nil {
		return nil, fmt.Errorf("read volumes directory: %w", err)
	}

	var ids []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Check if metadata.json exists
		metaPath := filepath.Join(volumesDir, entry.Name(), "metadata.json")
		if _, err := os.Stat(metaPath); err == nil {
			ids = append(ids, entry.Name())
		}
	}

	return ids, nil
}

