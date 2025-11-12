package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/onkernel/hypeman/lib/volumes"
)

// newTestService creates an ApiService for testing with automatic cleanup
func newTestService(t *testing.T) *ApiService {
	cfg := &config.Config{
		DataDir: t.TempDir(),
	}

	imageMgr, err := images.NewManager(cfg.DataDir, 1)
	if err != nil {
		t.Fatalf("failed to create image manager: %v", err)
	}

	systemMgr := system.NewManager(cfg.DataDir)
	maxOverlaySize := int64(100 * 1024 * 1024 * 1024) // 100GB for tests
	instanceMgr := instances.NewManager(cfg.DataDir, imageMgr, systemMgr, maxOverlaySize)
	volumeMgr := volumes.NewManager(cfg.DataDir)

	// Register cleanup for orphaned Cloud Hypervisor processes
	t.Cleanup(func() {
		cleanupOrphanedProcesses(t, cfg.DataDir)
	})

	return &ApiService{
		Config:          cfg,
		ImageManager:    imageMgr,
		InstanceManager: instanceMgr,
		VolumeManager:   volumeMgr,
	}
}

// cleanupOrphanedProcesses kills Cloud Hypervisor processes from metadata files
func cleanupOrphanedProcesses(t *testing.T, dataDir string) {
	guestsDir := filepath.Join(dataDir, "guests")
	
	entries, err := os.ReadDir(guestsDir)
	if err != nil {
		return // No guests directory
	}
	
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		
		metaPath := filepath.Join(guestsDir, entry.Name(), "metadata.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		
		// Parse just the CHPID field
		var meta struct {
			CHPID *int `json:"CHPID"`
		}
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}
		
		// If metadata has a PID, try to kill it
		if meta.CHPID != nil {
			pid := *meta.CHPID
			
			// Check if process exists
			if err := syscall.Kill(pid, 0); err == nil {
				t.Logf("Cleaning up orphaned Cloud Hypervisor process: PID %d", pid)
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
}

func ctx() context.Context {
	return context.Background()
}
