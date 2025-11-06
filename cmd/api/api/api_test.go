package api

import (
	"context"
	"testing"

	"github.com/onkernel/cloud-hypervisor-dataplane/cmd/api/config"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/images"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/instances"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/volumes"
)

// newTestService creates an ApiService for testing with temporary data directory
func newTestService(t *testing.T) *ApiService {
	cfg := &config.Config{
		DataDir: t.TempDir(),
	}

	return &ApiService{
		Config:          cfg,
		ImageManager:    images.NewManager(cfg.DataDir),
		InstanceManager: instances.NewManager(cfg.DataDir),
		VolumeManager:   volumes.NewManager(cfg.DataDir),
	}
}

func ctx() context.Context {
	return context.Background()
}

