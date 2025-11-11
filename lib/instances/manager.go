package instances

import (
	"context"
	"fmt"
)

type Manager interface {
	ListInstances(ctx context.Context) ([]Instance, error)
	CreateInstance(ctx context.Context, req CreateInstanceRequest) (*Instance, error)
	GetInstance(ctx context.Context, id string) (*Instance, error)
	DeleteInstance(ctx context.Context, id string) error
	StandbyInstance(ctx context.Context, id string) (*Instance, error)
	RestoreInstance(ctx context.Context, id string) (*Instance, error)
	GetInstanceLogs(ctx context.Context, id string, follow bool, tail int) (string, error)
	AttachVolume(ctx context.Context, id string, volumeId string, req AttachVolumeRequest) (*Instance, error)
	DetachVolume(ctx context.Context, id string, volumeId string) (*Instance, error)
}

type manager struct {
	dataDir string
}

func NewManager(dataDir string) Manager {
	return &manager{
		dataDir: dataDir,
	}
}

func (m *manager) ListInstances(ctx context.Context) ([]Instance, error) {
	return []Instance{}, nil
}

func (m *manager) CreateInstance(ctx context.Context, req CreateInstanceRequest) (*Instance, error) {
	return nil, fmt.Errorf("instance creation not yet implemented")
}

func (m *manager) GetInstance(ctx context.Context, id string) (*Instance, error) {
	return nil, ErrNotFound
}

func (m *manager) DeleteInstance(ctx context.Context, id string) error {
	return ErrNotFound
}

func (m *manager) StandbyInstance(ctx context.Context, id string) (*Instance, error) {
	return nil, fmt.Errorf("standby instance not yet implemented")
}

func (m *manager) RestoreInstance(ctx context.Context, id string) (*Instance, error) {
	return nil, fmt.Errorf("restore instance not yet implemented")
}

func (m *manager) GetInstanceLogs(ctx context.Context, id string, follow bool, tail int) (string, error) {
	return "", fmt.Errorf("get instance logs not yet implemented")
}

func (m *manager) AttachVolume(ctx context.Context, id string, volumeId string, req AttachVolumeRequest) (*Instance, error) {
	return nil, fmt.Errorf("attach volume not yet implemented")
}

func (m *manager) DetachVolume(ctx context.Context, id string, volumeId string) (*Instance, error) {
	return nil, fmt.Errorf("detach volume not yet implemented")
}
