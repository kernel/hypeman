package volumes

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/kernel/hypeman/lib/paths"
)

// nfsManager handles NFS export lifecycle for volumes that need ReadWriteMany access.
// NFS is an internal implementation detail — callers never see NFS configuration.
type nfsManager struct {
	paths   *paths.Paths
	mu      sync.Mutex
	exports map[string]bool // volumeID -> actively exported
}

func newNFSManager(p *paths.Paths) *nfsManager {
	return &nfsManager{
		paths:   p,
		exports: make(map[string]bool),
	}
}

// startServing sets up NFS export for a volume:
//  1. Loop-mounts data.raw to a host directory
//  2. Adds an NFS export entry
//  3. Refreshes the NFS export table
//
// Returns the export path on the host. The caller must combine this with the
// host gateway IP to form the full NFS mount spec for the guest.
func (n *nfsManager) startServing(volumeID string) (exportPath string, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.exports[volumeID] {
		// Already exported — return existing mount point
		return n.paths.VolumeNFSMount(volumeID), nil
	}

	mountDir := n.paths.VolumeNFSMount(volumeID)
	dataPath := n.paths.VolumeData(volumeID)

	// Create the mount point directory
	if err := os.MkdirAll(mountDir, 0755); err != nil {
		return "", fmt.Errorf("create nfs mount dir: %w", err)
	}

	// Loop-mount data.raw as ext4
	cmd := exec.Command("mount", "-o", "loop", "-t", "ext4", dataPath, mountDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("loop mount %s: %s: %w", dataPath, strings.TrimSpace(string(output)), err)
	}

	// Add NFS export (rw, no_root_squash for VM access, sync for data safety)
	exportLine := fmt.Sprintf("%s *(rw,no_root_squash,no_subtree_check,sync,fsid=%s)\n", mountDir, volumeID)
	if err := appendExport(exportLine); err != nil {
		// Cleanup: unmount on failure
		exec.Command("umount", mountDir).Run()
		return "", fmt.Errorf("add nfs export: %w", err)
	}

	// Refresh NFS exports
	if err := refreshExports(); err != nil {
		// Cleanup: remove export entry and unmount
		removeExport(mountDir)
		exec.Command("umount", mountDir).Run()
		return "", fmt.Errorf("refresh nfs exports: %w", err)
	}

	n.exports[volumeID] = true
	return mountDir, nil
}

// stopServing tears down NFS export and unmounts the volume.
func (n *nfsManager) stopServing(volumeID string) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if !n.exports[volumeID] {
		return nil // Not exported, nothing to do
	}

	mountDir := n.paths.VolumeNFSMount(volumeID)

	// Remove NFS export
	removeExport(mountDir)
	refreshExports()

	// Unmount
	cmd := exec.Command("umount", mountDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("unmount %s: %s: %w", mountDir, strings.TrimSpace(string(output)), err)
	}

	// Clean up mount directory
	os.Remove(mountDir)

	delete(n.exports, volumeID)
	return nil
}

// isServing returns whether a volume is currently NFS-exported.
func (n *nfsManager) isServing(volumeID string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.exports[volumeID]
}

const exportsFile = "/etc/exports"

// appendExport adds a line to /etc/exports.
func appendExport(line string) error {
	f, err := os.OpenFile(exportsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}

// removeExport removes all lines containing mountDir from /etc/exports.
func removeExport(mountDir string) error {
	data, err := os.ReadFile(exportsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.Contains(line, mountDir) {
			continue // Drop this export
		}
		kept = append(kept, line)
	}

	return os.WriteFile(exportsFile, []byte(strings.Join(kept, "\n")+"\n"), 0644)
}

// refreshExports runs exportfs -ra to apply /etc/exports changes.
func refreshExports() error {
	cmd := exec.Command("exportfs", "-ra")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("exportfs -ra: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}
