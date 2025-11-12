package api

import (
	"context"
	"testing"

	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/onkernel/hypeman/lib/volumes"
)

// newTestService creates an ApiService for testing with temporary data directory
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

	return &ApiService{
		Config:          cfg,
		ImageManager:    imageMgr,
		InstanceManager: instanceMgr,
		VolumeManager:   volumeMgr,
	}
}

func ctx() context.Context {
	return context.Background()
}
