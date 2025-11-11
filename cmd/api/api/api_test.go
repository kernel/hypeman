package api

import (
	"context"
	"testing"

	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/instances"
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

	return &ApiService{
		Config:          cfg,
		ImageManager:    imageMgr,
		InstanceManager: instances.NewManager(cfg.DataDir),
		VolumeManager:   volumes.NewManager(cfg.DataDir),
	}
}

func ctx() context.Context {
	return context.Background()
}
