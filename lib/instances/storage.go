package instances

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Filesystem structure:
// {dataDir}/guests/{instance-id}/
//   metadata.json      # Instance metadata
//   overlay.raw        # Configurable sparse overlay disk (default 10GB)
//   config.ext4        # Read-only config disk (generated)
//   ch.sock            # Cloud Hypervisor API socket
//   ch-stdout.log      # CH process output
//   logs/
//     console.log      # Serial console output
//   snapshots/
//     snapshot-latest/ # Snapshot directory
//       vm.json
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

	// Create sparse file
	file, err := os.Create(overlayPath)
	if err != nil {
		return fmt.Errorf("create overlay disk: %w", err)
	}
	file.Close()

	// Truncate to specified size to create sparse file
	if err := os.Truncate(overlayPath, sizeBytes); err != nil {
		return fmt.Errorf("truncate overlay disk: %w", err)
	}

	// Format as ext4 (VM will mount this as writable overlay)
	cmd := exec.Command("mkfs.ext4", "-F", overlayPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4 on overlay: %w, output: %s", err, output)
	}

	return nil
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

