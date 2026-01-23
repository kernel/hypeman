package api

import (
	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/builds"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/ingress"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/resources"
	"github.com/kernel/hypeman/lib/vm_metrics"
	"github.com/kernel/hypeman/lib/volumes"
)

// ApiService implements the oapi.StrictServerInterface
type ApiService struct {
	Config           *config.Config
	ImageManager     images.Manager
	InstanceManager  instances.Manager
	VolumeManager    volumes.Manager
	NetworkManager   network.Manager
	DeviceManager    devices.Manager
	IngressManager   ingress.Manager
	BuildManager     builds.Manager
	ResourceManager  *resources.Manager
	VMMetricsManager *vm_metrics.Manager
}

var _ oapi.StrictServerInterface = (*ApiService)(nil)

// New creates a new ApiService
func New(
	config *config.Config,
	imageManager images.Manager,
	instanceManager instances.Manager,
	volumeManager volumes.Manager,
	networkManager network.Manager,
	deviceManager devices.Manager,
	ingressManager ingress.Manager,
	buildManager builds.Manager,
	resourceManager *resources.Manager,
	vmMetricsManager *vm_metrics.Manager,
) *ApiService {
	return &ApiService{
		Config:           config,
		ImageManager:     imageManager,
		InstanceManager:  instanceManager,
		VolumeManager:    volumeManager,
		NetworkManager:   networkManager,
		DeviceManager:    deviceManager,
		IngressManager:   ingressManager,
		BuildManager:     buildManager,
		ResourceManager:  resourceManager,
		VMMetricsManager: vmMetricsManager,
	}
}
