//go:build wireinject

package main

import (
	"context"
	"log/slog"

	"github.com/google/wire"
	"github.com/kernel/hypeman/cmd/api/api"
	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/builds"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/ingress"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/providers"
	"github.com/kernel/hypeman/lib/registry"
	"github.com/kernel/hypeman/lib/resources"
	"github.com/kernel/hypeman/lib/system"
	"github.com/kernel/hypeman/lib/volumes"
)

// application struct to hold initialized components
type application struct {
	Ctx             context.Context
	Logger          *slog.Logger
	Config          *config.Config
	ImageManager    images.Manager
	SystemManager   system.Manager
	NetworkManager  network.Manager
	DeviceManager   devices.Manager
	InstanceManager instances.Manager
	VolumeManager   volumes.Manager
	IngressManager  ingress.Manager
	BuildManager    builds.Manager
	ResourceManager *resources.Manager
	Registry        *registry.Registry
	ApiService      *api.ApiService
}

// initializeApp is the injector function
func initializeApp() (*application, func(), error) {
	panic(wire.Build(
		providers.ProvideLogger,
		providers.ProvideContext,
		providers.ProvideConfig,
		providers.ProvidePaths,
		providers.ProvideImageManager,
		providers.ProvideSystemManager,
		providers.ProvideNetworkManager,
		providers.ProvideDeviceManager,
		providers.ProvideInstanceManager,
		providers.ProvideVolumeManager,
		providers.ProvideIngressManager,
		providers.ProvideBuildManager,
		providers.ProvideResourceManager,
		providers.ProvideRegistry,
		api.New,
		wire.Struct(new(application), "*"),
	))
}
