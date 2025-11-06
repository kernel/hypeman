package providers

import (
	"context"
	"log/slog"
	"os"

	"github.com/onkernel/cloud-hypervisor-dataplane/cmd/api/config"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/images"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/instances"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/volumes"
)

// ProvideContext provides a base context
func ProvideContext() context.Context {
	return context.Background()
}

// ProvideLogger provides a structured logger
func ProvideLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}

// ProvideConfig provides the application configuration
func ProvideConfig() *config.Config {
	return config.Load()
}

// ProvideImageManager provides the image manager
func ProvideImageManager(cfg *config.Config) images.Manager {
	return images.NewManager(cfg.DataDir)
}

// ProvideInstanceManager provides the instance manager
func ProvideInstanceManager(cfg *config.Config) instances.Manager {
	return instances.NewManager(cfg.DataDir)
}

// ProvideVolumeManager provides the volume manager
func ProvideVolumeManager(cfg *config.Config) volumes.Manager {
	return volumes.NewManager(cfg.DataDir)
}

