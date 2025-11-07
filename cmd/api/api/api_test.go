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

	// Create OCI client for testing
	ociClient, err := images.NewOCIClient(cfg.DataDir + "/system/oci-cache")
	if err != nil {
		t.Fatalf("failed to create OCI client: %v", err)
	}

	return &ApiService{
		Config:          cfg,
		ImageManager:    images.NewManager(cfg.DataDir, ociClient, 1),
		InstanceManager: instances.NewManager(cfg.DataDir),
		VolumeManager:   volumes.NewManager(cfg.DataDir),
	}
}

func ctx() context.Context {
	return context.Background()
}
