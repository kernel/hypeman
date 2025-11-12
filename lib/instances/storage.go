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

// metadata wraps Instance with additional serialization
type metadata struct {
	Instance
}

// ToInstance converts metadata to Instance
func (m *metadata) ToInstance() *Instance {
	inst := m.Instance
	return &inst
}

// ensureDirectories creates the instance directory structure
func (m *manager) ensureDirectories(id string) error {
	instDir := filepath.Join(m.dataDir, "guests", id)

	dirs := []string{
		instDir,
		filepath.Join(instDir, "logs"),
		filepath.Join(instDir, "snapshots"),
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
	metaPath := filepath.Join(m.dataDir, "guests", id, "metadata.json")

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
	metaPath := filepath.Join(m.dataDir, "guests", meta.Id, "metadata.json")

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
	overlayPath := filepath.Join(m.dataDir, "guests", id, "overlay.raw")

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
	instDir := filepath.Join(m.dataDir, "guests", id)

	if err := os.RemoveAll(instDir); err != nil {
		return fmt.Errorf("remove instance directory: %w", err)
	}

	return nil
}

// listMetadataFiles returns paths to all instance metadata files
func (m *manager) listMetadataFiles() ([]string, error) {
	guestsDir := filepath.Join(m.dataDir, "guests")

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

