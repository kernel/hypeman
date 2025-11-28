package providers

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/c2h5oh/datasize"
	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/devices"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/network"
	"github.com/onkernel/hypeman/lib/paths"
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

// ProvidePaths provides the paths abstraction
func ProvidePaths(cfg *config.Config) *paths.Paths {
	return paths.New(cfg.DataDir)
}

// ProvideImageManager provides the image manager
func ProvideImageManager(p *paths.Paths, cfg *config.Config) (images.Manager, error) {
	return images.NewManager(p, cfg.MaxConcurrentBuilds)
}

// ProvideSystemManager provides the system manager
func ProvideSystemManager(p *paths.Paths) system.Manager {
	return system.NewManager(p)
}

// ProvideNetworkManager provides the network manager
func ProvideNetworkManager(p *paths.Paths, cfg *config.Config) network.Manager {
	return network.NewManager(p, cfg)
}

// ProvideDeviceManager provides the device manager
func ProvideDeviceManager(p *paths.Paths) devices.Manager {
	return devices.NewManager(p)
}

// ProvideInstanceManager provides the instance manager
func ProvideInstanceManager(p *paths.Paths, cfg *config.Config, imageManager images.Manager, systemManager system.Manager, networkManager network.Manager, deviceManager devices.Manager) (instances.Manager, error) {
	// Parse max overlay size from config
	var maxOverlaySize datasize.ByteSize
	if err := maxOverlaySize.UnmarshalText([]byte(cfg.MaxOverlaySize)); err != nil {
		return nil, fmt.Errorf("failed to parse MAX_OVERLAY_SIZE '%s': %w (expected format like '100GB', '50G', '10GiB')", cfg.MaxOverlaySize, err)
	}
	return instances.NewManager(p, imageManager, systemManager, networkManager, deviceManager, int64(maxOverlaySize)), nil
}

// ProvideVolumeManager provides the volume manager
func ProvideVolumeManager(p *paths.Paths) volumes.Manager {
	return volumes.NewManager(p)
}
