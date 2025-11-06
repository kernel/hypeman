package api

import (
	"github.com/onkernel/cloud-hypervisor-dataplane/cmd/api/config"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/images"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/instances"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/oapi"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/volumes"
)

// ApiService implements the oapi.StrictServerInterface
type ApiService struct {
	Config          *config.Config
	ImageManager    images.Manager
	InstanceManager instances.Manager
	VolumeManager   volumes.Manager
}

var _ oapi.StrictServerInterface = (*ApiService)(nil)

// New creates a new ApiService
func New(
	config *config.Config,
	imageManager images.Manager,
	instanceManager instances.Manager,
	volumeManager volumes.Manager,
) *ApiService {
	return &ApiService{
		Config:          config,
		ImageManager:    imageManager,
		InstanceManager: instanceManager,
		VolumeManager:   volumeManager,
	}
}

