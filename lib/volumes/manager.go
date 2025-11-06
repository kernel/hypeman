package volumes

import (
	"context"
	"fmt"

	"github.com/onkernel/cloud-hypervisor-dataplane/lib/oapi"
)

// Manager handles volume lifecycle operations
type Manager interface {
	ListVolumes(ctx context.Context) ([]oapi.Volume, error)
	CreateVolume(ctx context.Context, req oapi.CreateVolumeRequest) (*oapi.Volume, error)
	GetVolume(ctx context.Context, id string) (*oapi.Volume, error)
	DeleteVolume(ctx context.Context, id string) error
}

type manager struct {
	dataDir string
}

// NewManager creates a new volume manager
func NewManager(dataDir string) Manager {
	return &manager{
		dataDir: dataDir,
	}
}

func (m *manager) ListVolumes(ctx context.Context) ([]oapi.Volume, error) {
	// TODO: implement
	return []oapi.Volume{}, nil
}

func (m *manager) CreateVolume(ctx context.Context, req oapi.CreateVolumeRequest) (*oapi.Volume, error) {
	// TODO: implement
	return nil, fmt.Errorf("volume creation not yet implemented")
}

func (m *manager) GetVolume(ctx context.Context, id string) (*oapi.Volume, error) {
	// TODO: implement
	return nil, fmt.Errorf("get volume not yet implemented")
}

func (m *manager) DeleteVolume(ctx context.Context, id string) error {
	// TODO: implement
	return fmt.Errorf("delete volume not yet implemented")
}

