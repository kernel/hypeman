package api

import (
	"context"
	"encoding/json"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/network"
	"github.com/onkernel/hypeman/lib/oapi"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/onkernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/require"
)

// newTestService creates an ApiService for testing with automatic cleanup
func newTestService(t *testing.T) *ApiService {
	cfg := &config.Config{
		DataDir: t.TempDir(),
	}

	p := paths.New(cfg.DataDir)
	imageMgr, err := images.NewManager(p, 1)
	if err != nil {
		t.Fatalf("failed to create image manager: %v", err)
	}

	systemMgr := system.NewManager(p)
	networkMgr := network.NewManager(p, cfg)
	volumeMgr := volumes.NewManager(p, 0) // 0 = unlimited storage
	limits := instances.ResourceLimits{
		MaxOverlaySize: 100 * 1024 * 1024 * 1024, // 100GB
	}
	instanceMgr := instances.NewManager(p, imageMgr, systemMgr, networkMgr, volumeMgr, limits)

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
	p := paths.New(dataDir)
	guestsDir := p.GuestsDir()

	entries, err := os.ReadDir(guestsDir)
	if err != nil {
		return // No guests directory
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		metaPath := p.InstanceMetadata(entry.Name())
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

// createAndWaitForImage creates an image and waits for it to be ready.
// Returns the image name on success, or fails the test on error/timeout.
func createAndWaitForImage(t *testing.T, svc *ApiService, imageName string, timeout time.Duration) string {
	t.Helper()

	t.Logf("Creating image %s...", imageName)
	imgResp, err := svc.CreateImage(ctx(), oapi.CreateImageRequestObject{
		Body: &oapi.CreateImageRequest{
			Name: imageName,
		},
	})
	require.NoError(t, err)

	imgCreated, ok := imgResp.(oapi.CreateImage202JSONResponse)
	require.True(t, ok, "expected 202 response for image creation")

	t.Log("Waiting for image to be ready...")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		imgResp, err := svc.GetImage(ctx(), oapi.GetImageRequestObject{
			Name: imageName,
		})
		require.NoError(t, err)

		img, ok := imgResp.(oapi.GetImage200JSONResponse)
		if ok {
			switch img.Status {
			case "ready":
				t.Log("Image is ready")
				return imgCreated.Name
			case "failed":
				t.Fatalf("Image build failed: %v", img.Error)
			default:
				t.Logf("Image status: %s", img.Status)
			}
		}
		time.Sleep(1 * time.Second)
	}

	t.Fatalf("Timeout waiting for image %s to be ready", imageName)
	return ""
}
