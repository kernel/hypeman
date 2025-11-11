package volumes

import (
	"context"
	"fmt"
)

type Manager interface {
	ListVolumes(ctx context.Context) ([]Volume, error)
	CreateVolume(ctx context.Context, req CreateVolumeRequest) (*Volume, error)
	GetVolume(ctx context.Context, id string) (*Volume, error)
	DeleteVolume(ctx context.Context, id string) error
}

type manager struct {
	dataDir string
}

func NewManager(dataDir string) Manager {
	return &manager{
		dataDir: dataDir,
	}
}

func (m *manager) ListVolumes(ctx context.Context) ([]Volume, error) {
	return []Volume{}, nil
}

func (m *manager) CreateVolume(ctx context.Context, req CreateVolumeRequest) (*Volume, error) {
	return nil, fmt.Errorf("volume creation not yet implemented")
}

func (m *manager) GetVolume(ctx context.Context, id string) (*Volume, error) {
	return nil, ErrNotFound
}

func (m *manager) DeleteVolume(ctx context.Context, id string) error {
	return ErrNotFound
}
