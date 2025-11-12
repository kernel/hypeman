package providers

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/c2h5oh/datasize"
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
func ProvideInstanceManager(cfg *config.Config, imageManager images.Manager, systemManager system.Manager) (instances.Manager, error) {
	// Parse max overlay size from config
	var maxOverlaySize datasize.ByteSize
	if err := maxOverlaySize.UnmarshalText([]byte(cfg.MaxOverlaySize)); err != nil {
		return nil, fmt.Errorf("failed to parse MAX_OVERLAY_SIZE '%s': %w (expected format like '100GB', '50G', '10GiB')", cfg.MaxOverlaySize, err)
	}
	return instances.NewManager(cfg.DataDir, imageManager, systemManager, int64(maxOverlaySize)), nil
}

// ProvideVolumeManager provides the volume manager
func ProvideVolumeManager(cfg *config.Config) volumes.Manager {
	return volumes.NewManager(cfg.DataDir)
}
