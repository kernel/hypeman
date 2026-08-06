package instances

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/healthcheck"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
)

const (
	deleteInstanceDataMaxRetries     = 5
	deleteInstanceDataInitialBackoff = 10 * time.Millisecond
	deleteInstanceDataMaxBackoff     = 100 * time.Millisecond
)

// Filesystem structure:
// {dataDir}/guests/{instance-id}/
//   metadata.json      # Instance metadata
//   overlay.raw        # Configurable sparse overlay disk (default 10GB)
//   config.ext4        # Read-only config disk (generated)
//   ch.sock            # Hypervisor API socket (abbreviated name for SUN_LEN limit)
//   logs/
//     app.log          # Guest application log (serial console output)
//     vmm.log          # Hypervisor log (stdout+stderr combined)
//     hypeman.log      # Hypeman operations log (actions taken on this instance)
//   snapshots/
//     snapshot-latest/ # Snapshot directory
//       config.json
//       memory-ranges

// metadata wraps StoredMetadata for JSON serialization
type metadata struct {
	StoredMetadata
	AutoStandbyRuntime *autostandby.Runtime `json:"auto_standby_runtime,omitempty"`
	HealthCheckRuntime *healthcheck.Runtime `json:"health_check_runtime,omitempty"`
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
	unlockAliasReaders := hypervisor.LockSnapshotSourceAliasReaders()
	defer unlockAliasReaders()

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
	unlockAliasReaders := hypervisor.LockSnapshotSourceAliasReaders()
	defer unlockAliasReaders()

	metaPath := m.paths.InstanceMetadata(meta.Id)

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(metaPath), ".metadata-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary metadata: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temporary metadata: %w", err)
	}
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temporary metadata: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary metadata: %w", err)
	}

	if err := os.Rename(tmpPath, metaPath); err != nil {
		return fmt.Errorf("rename metadata: %w", err)
	}

	m.syncAdmissionAllocation(meta)

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

	if err := removeAllWithRetry(instDir, os.RemoveAll, time.Sleep); err != nil {
		return fmt.Errorf("remove instance directory: %w", err)
	}

	m.deleteAdmissionAllocation(id)

	return nil
}

func removeAllWithRetry(path string, removeAll func(string) error, sleep func(time.Duration)) error {
	backoff := deleteInstanceDataInitialBackoff

	for attempt := 0; ; attempt++ {
		err := removeAll(path)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.ENOTEMPTY) || attempt >= deleteInstanceDataMaxRetries {
			return err
		}

		sleep(backoff)
		backoff *= 2
		if backoff > deleteInstanceDataMaxBackoff {
			backoff = deleteInstanceDataMaxBackoff
		}
	}
}

// listMetadataFiles returns paths to all instance metadata files.
func (m *manager) listMetadataFiles() ([]string, error) {
	return m.listMetadataFilesWithStatErrors(false)
}

func (m *manager) listMetadataFilesWithStatErrors(failOnStatError bool) ([]string, error) {
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
		} else if failOnStatError && !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat metadata for instance %s: %w", entry.Name(), err)
		}
	}

	return metaFiles, nil
}
