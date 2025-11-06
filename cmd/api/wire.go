// +build wireinject

package main

import (
	"context"
	"log/slog"

	"github.com/google/wire"
	"github.com/onkernel/cloud-hypervisor-dataplane/cmd/api/api"
	"github.com/onkernel/cloud-hypervisor-dataplane/cmd/api/config"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/images"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/instances"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/providers"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/volumes"
)

// application struct to hold initialized components
type application struct {
	Ctx             context.Context
	Logger          *slog.Logger
	Config          *config.Config
	ImageManager    images.Manager
	InstanceManager instances.Manager
	VolumeManager   volumes.Manager
	ApiService      *api.ApiService
}

// initializeApp is the injector function
func initializeApp() (*application, func(), error) {
	panic(wire.Build(
		providers.ProvideContext,
		providers.ProvideLogger,
		providers.ProvideConfig,
		providers.ProvideImageManager,
		providers.ProvideInstanceManager,
		providers.ProvideVolumeManager,
		api.New,
		wire.Struct(new(application), "*"),
	))
}

