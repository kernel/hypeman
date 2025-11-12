// +build wireinject

package main

import (
	"context"
	"log/slog"

	"github.com/google/wire"
	"github.com/onkernel/hypeman/cmd/api/api"
	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/providers"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/onkernel/hypeman/lib/volumes"
)

// application struct to hold initialized components
type application struct {
	Ctx             context.Context
	Logger          *slog.Logger
	Config          *config.Config
	ImageManager    images.Manager
	SystemManager   system.Manager
	InstanceManager instances.Manager
	VolumeManager   volumes.Manager
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
		providers.ProvideInstanceManager,
		providers.ProvideVolumeManager,
		api.New,
		wire.Struct(new(application), "*"),
	))
}

