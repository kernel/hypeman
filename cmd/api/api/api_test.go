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
	mw "github.com/onkernel/hypeman/lib/middleware"
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
	imageMgr, err := images.NewManager(p, 1, nil)
	if err != nil {
		t.Fatalf("failed to create image manager: %v", err)
	}

	systemMgr := system.NewManager(p)
	networkMgr := network.NewManager(p, cfg, nil)
	volumeMgr := volumes.NewManager(p, 0, nil) // 0 = unlimited storage
	limits := instances.ResourceLimits{
		MaxOverlaySize: 100 * 1024 * 1024 * 1024, // 100GB
	}
	instanceMgr := instances.NewManager(p, imageMgr, systemMgr, networkMgr, volumeMgr, limits, nil, nil)

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

// ctxWithInstance creates a context with a resolved instance (simulates ResolveResource middleware)
func ctxWithInstance(svc *ApiService, idOrName string) context.Context {
	inst, err := svc.InstanceManager.GetInstance(ctx(), idOrName)
	if err != nil {
		return ctx() // Let handler deal with the error
	}
	return mw.WithResolvedInstance(ctx(), inst.Id, inst)
}

// ctxWithVolume creates a context with a resolved volume (simulates ResolveResource middleware)
func ctxWithVolume(svc *ApiService, idOrName string) context.Context {
	vol, err := svc.VolumeManager.GetVolume(ctx(), idOrName)
	if err != nil {
		vol, err = svc.VolumeManager.GetVolumeByName(ctx(), idOrName)
	}
	if err != nil {
		return ctx()
	}
	return mw.WithResolvedVolume(ctx(), vol.Id, vol)
}

// ctxWithImage creates a context with a resolved image (simulates ResolveResource middleware)
func ctxWithImage(svc *ApiService, name string) context.Context {
	img, err := svc.ImageManager.GetImage(ctx(), name)
	if err != nil {
		return ctx()
	}
	return mw.WithResolvedImage(ctx(), img.Name, img)
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
		// Get image from manager (may fail during pending/pulling, that's OK)
		img, err := svc.ImageManager.GetImage(ctx(), imageName)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		switch img.Status {
		case "ready":
			t.Log("Image is ready")
			return imgCreated.Name
		case "failed":
			errMsg := ""
			if img.Error != nil {
				errMsg = *img.Error
			}
			t.Fatalf("Image build failed: %v", errMsg)
		}
		// Still pending/pulling/converting, poll again
		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("Timeout waiting for image %s to be ready", imageName)
	return ""
}
