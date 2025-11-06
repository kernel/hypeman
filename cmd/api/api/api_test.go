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

	// Create Docker client for testing (may be nil if not available)
	dockerClient, _ := images.NewDockerClient()

	return &ApiService{
		Config:          cfg,
		ImageManager:    images.NewManager(cfg.DataDir, dockerClient),
		InstanceManager: instances.NewManager(cfg.DataDir),
		VolumeManager:   volumes.NewManager(cfg.DataDir),
	}
}

func ctx() context.Context {
	return context.Background()
}
