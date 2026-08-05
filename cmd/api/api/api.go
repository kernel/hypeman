package api

import (
	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/builders"
	"github.com/kernel/hypeman/lib/builds"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/guestmemory"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/imagepush"
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
	Config                *config.Config
	ImageManager          images.Manager
	InstanceManager       instances.Manager
	VolumeManager         volumes.Manager
	BuilderManager        builders.Manager
	NetworkManager        network.Manager
	DeviceManager         devices.Manager
	IngressManager        ingress.Manager
	BuildManager          builds.Manager
	PushManager           imagepush.Manager
	ResourceManager       *resources.Manager
	GuestMemoryController guestmemory.Controller
	AutoStandbyController *autostandby.Controller
	VMMetricsManager      *vm_metrics.Manager
}

var _ oapi.StrictServerInterface = (*ApiService)(nil)

// New creates a new ApiService
func New(
	config *config.Config,
	imageManager images.Manager,
	instanceManager instances.Manager,
	volumeManager volumes.Manager,
	builderManager builders.Manager,
	networkManager network.Manager,
	deviceManager devices.Manager,
	ingressManager ingress.Manager,
	buildManager builds.Manager,
	pushManager imagepush.Manager,
	resourceManager *resources.Manager,
	guestMemoryController guestmemory.Controller,
	autoStandbyController *autostandby.Controller,
	vmMetricsManager *vm_metrics.Manager,
) *ApiService {
	return &ApiService{
		Config:                config,
		ImageManager:          imageManager,
		InstanceManager:       instanceManager,
		VolumeManager:         volumeManager,
		BuilderManager:        builderManager,
		NetworkManager:        networkManager,
		DeviceManager:         deviceManager,
		IngressManager:        ingressManager,
		BuildManager:          buildManager,
		PushManager:           pushManager,
		ResourceManager:       resourceManager,
		GuestMemoryController: guestMemoryController,
		AutoStandbyController: autoStandbyController,
		VMMetricsManager:      vmMetricsManager,
	}
}
