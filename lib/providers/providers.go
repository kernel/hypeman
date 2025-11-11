package providers

import (
	"context"
	"log/slog"
	"os"

	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/onkernel/hypeman/lib/volumes"
)

// ProvideLogger provides a structured logger
func ProvideLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}

// ProvideContext provides a context with logger attached
func ProvideContext(log *slog.Logger) context.Context {
	return logger.AddToContext(context.Background(), log)
}

// ProvideConfig provides the application configuration
func ProvideConfig() *config.Config {
	return config.Load()
}

// ProvideImageManager provides the image manager
func ProvideImageManager(cfg *config.Config) (images.Manager, error) {
	return images.NewManager(cfg.DataDir, cfg.MaxConcurrentBuilds)
}

// ProvideSystemManager provides the system manager
func ProvideSystemManager(cfg *config.Config) system.Manager {
	return system.NewManager(cfg.DataDir)
}

// ProvideInstanceManager provides the instance manager
func ProvideInstanceManager(cfg *config.Config, imageManager images.Manager, systemManager system.Manager) instances.Manager {
	return instances.NewManager(cfg.DataDir, imageManager, systemManager)
}

// ProvideVolumeManager provides the volume manager
func ProvideVolumeManager(cfg *config.Config) volumes.Manager {
	return volumes.NewManager(cfg.DataDir)
}
