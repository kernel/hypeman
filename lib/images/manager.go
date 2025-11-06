package images

import (
	"context"
	"fmt"

	"github.com/onkernel/cloud-hypervisor-dataplane/lib/oapi"
)

// Manager handles image lifecycle operations
type Manager interface {
	ListImages(ctx context.Context) ([]oapi.Image, error)
	CreateImage(ctx context.Context, req oapi.CreateImageRequest) (*oapi.Image, error)
	GetImage(ctx context.Context, id string) (*oapi.Image, error)
	DeleteImage(ctx context.Context, id string) error
}

type manager struct {
	dataDir string
}

// NewManager creates a new image manager
func NewManager(dataDir string) Manager {
	return &manager{
		dataDir: dataDir,
	}
}

func (m *manager) ListImages(ctx context.Context) ([]oapi.Image, error) {
	// TODO: implement
	return []oapi.Image{}, nil
}

func (m *manager) CreateImage(ctx context.Context, req oapi.CreateImageRequest) (*oapi.Image, error) {
	// TODO: implement
	return nil, fmt.Errorf("image creation not yet implemented")
}

func (m *manager) GetImage(ctx context.Context, id string) (*oapi.Image, error) {
	// TODO: implement
	return nil, fmt.Errorf("get image not yet implemented")
}

func (m *manager) DeleteImage(ctx context.Context, id string) error {
	// TODO: implement
	return fmt.Errorf("delete image not yet implemented")
}

