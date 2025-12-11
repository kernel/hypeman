package instances

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/onkernel/hypeman/lib/images"
)

// Filesystem structure:
// {dataDir}/guests/{instance-id}/
//   metadata.json      # Instance metadata
//   overlay.raw        # Configurable sparse overlay disk (default 10GB)
//   config.ext4        # Read-only config disk (generated)
//   ch.sock            # Cloud Hypervisor API socket
//   logs/
//     app.log          # Guest application log (serial console output)
//     vmm.log          # Cloud Hypervisor VMM log (stdout+stderr combined)
//     hypeman.log      # Hypeman operations log (actions taken on this instance)
//   snapshots/
//     snapshot-latest/ # Snapshot directory
//       config.json
//       memory-ranges

// metadata wraps StoredMetadata for JSON serialization
type metadata struct {
	StoredMetadata
}

// ensureDirectories creates the instance directory structure
func (m *manager) ensureDirectories(id string) error {
	dirs := []string{
		m.paths.InstanceDir(id),
		m.paths.InstanceLogs(id),
		m.paths.InstanceSnapshots(id),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}

	return nil
}

// loadMetadata loads instance metadata from disk
func (m *manager) loadMetadata(id string) (*metadata, error) {
	metaPath := m.paths.InstanceMetadata(id)

	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read metadata: %w", err)
	}

	var meta metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}

	return &meta, nil
}

// saveMetadata saves instance metadata to disk
func (m *manager) saveMetadata(meta *metadata) error {
	metaPath := m.paths.InstanceMetadata(meta.Id)

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	if err := os.WriteFile(metaPath, data, 0644); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}

	return nil
}

// createOverlayDisk creates a sparse overlay disk for the instance
func (m *manager) createOverlayDisk(id string, sizeBytes int64) error {
	overlayPath := m.paths.InstanceOverlay(id)
	return images.CreateEmptyExt4Disk(overlayPath, sizeBytes)
}

// createVolumeOverlayDisk creates a sparse overlay disk for a volume attachment.
// Cleanup note: If instance creation fails after this point, the overlay disk is
// cleaned up automatically by deleteInstanceData() which removes the entire instance
// directory (including vol-overlays/) via the cleanup stack in createInstance().
func (m *manager) createVolumeOverlayDisk(instanceID, volumeID string, sizeBytes int64) error {
	// Ensure vol-overlays directory exists
	overlaysDir := m.paths.InstanceVolumeOverlaysDir(instanceID)
	if err := os.MkdirAll(overlaysDir, 0755); err != nil {
		return fmt.Errorf("create vol-overlays directory: %w", err)
	}

	overlayPath := m.paths.InstanceVolumeOverlay(instanceID, volumeID)
	return images.CreateEmptyExt4Disk(overlayPath, sizeBytes)
}

// deleteInstanceData removes all instance data from disk
func (m *manager) deleteInstanceData(id string) error {
	instDir := m.paths.InstanceDir(id)

	if err := os.RemoveAll(instDir); err != nil {
		return fmt.Errorf("remove instance directory: %w", err)
	}

	return nil
}

// listMetadataFiles returns paths to all instance metadata files
func (m *manager) listMetadataFiles() ([]string, error) {
	guestsDir := m.paths.GuestsDir()

	// Ensure guests directory exists
	if err := os.MkdirAll(guestsDir, 0755); err != nil {
		return nil, fmt.Errorf("create guests directory: %w", err)
	}

	entries, err := os.ReadDir(guestsDir)
	if err != nil {
		return nil, fmt.Errorf("read guests directory: %w", err)
	}

	var metaFiles []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		metaPath := filepath.Join(guestsDir, entry.Name(), "metadata.json")
		if _, err := os.Stat(metaPath); err == nil {
			metaFiles = append(metaFiles, metaPath)
		}
	}

	return metaFiles, nil
}
