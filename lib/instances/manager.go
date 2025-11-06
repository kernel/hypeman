package instances

import (
	"context"
	"fmt"

	"github.com/onkernel/cloud-hypervisor-dataplane/lib/oapi"
)

// Manager handles instance lifecycle operations
type Manager interface {
	ListInstances(ctx context.Context) ([]oapi.Instance, error)
	CreateInstance(ctx context.Context, req oapi.CreateInstanceRequest) (*oapi.Instance, error)
	GetInstance(ctx context.Context, id string) (*oapi.Instance, error)
	DeleteInstance(ctx context.Context, id string) error
	StandbyInstance(ctx context.Context, id string) (*oapi.Instance, error)
	RestoreInstance(ctx context.Context, id string) (*oapi.Instance, error)
	GetInstanceLogs(ctx context.Context, id string, follow bool, tail int) (string, error)
	AttachVolume(ctx context.Context, id string, volumeId string, req oapi.AttachVolumeRequest) (*oapi.Instance, error)
	DetachVolume(ctx context.Context, id string, volumeId string) (*oapi.Instance, error)
}

type manager struct {
	dataDir string
}

// NewManager creates a new instance manager
func NewManager(dataDir string) Manager {
	return &manager{
		dataDir: dataDir,
	}
}

func (m *manager) ListInstances(ctx context.Context) ([]oapi.Instance, error) {
	// TODO: implement
	return []oapi.Instance{}, nil
}

func (m *manager) CreateInstance(ctx context.Context, req oapi.CreateInstanceRequest) (*oapi.Instance, error) {
	// TODO: implement
	return nil, fmt.Errorf("instance creation not yet implemented")
}

func (m *manager) GetInstance(ctx context.Context, id string) (*oapi.Instance, error) {
	// TODO: implement
	return nil, fmt.Errorf("get instance not yet implemented")
}

func (m *manager) DeleteInstance(ctx context.Context, id string) error {
	// TODO: implement
	return fmt.Errorf("delete instance not yet implemented")
}

func (m *manager) StandbyInstance(ctx context.Context, id string) (*oapi.Instance, error) {
	// TODO: implement
	return nil, fmt.Errorf("standby instance not yet implemented")
}

func (m *manager) RestoreInstance(ctx context.Context, id string) (*oapi.Instance, error) {
	// TODO: implement
	return nil, fmt.Errorf("restore instance not yet implemented")
}

func (m *manager) GetInstanceLogs(ctx context.Context, id string, follow bool, tail int) (string, error) {
	// TODO: implement
	return "", fmt.Errorf("get instance logs not yet implemented")
}

func (m *manager) AttachVolume(ctx context.Context, id string, volumeId string, req oapi.AttachVolumeRequest) (*oapi.Instance, error) {
	// TODO: implement
	return nil, fmt.Errorf("attach volume not yet implemented")
}

func (m *manager) DetachVolume(ctx context.Context, id string, volumeId string) (*oapi.Instance, error) {
	// TODO: implement
	return nil, fmt.Errorf("detach volume not yet implemented")
}

