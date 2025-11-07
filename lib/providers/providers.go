package providers

import (
	"context"
	"log/slog"
	"os"

	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/logger"
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

// ProvideOCIClient provides an OCI client
func ProvideOCIClient(cfg *config.Config) (*images.OCIClient, error) {
	// Use a cache directory under dataDir for OCI layouts
	cacheDir := cfg.DataDir + "/system/oci-cache"
	return images.NewOCIClient(cacheDir)
}

// ProvideImageManager provides the image manager
func ProvideImageManager(cfg *config.Config, ociClient *images.OCIClient) images.Manager {
	return images.NewManager(cfg.DataDir, ociClient, cfg.MaxConcurrentBuilds)
}

// ProvideInstanceManager provides the instance manager
func ProvideInstanceManager(cfg *config.Config) instances.Manager {
	return instances.NewManager(cfg.DataDir)
}

// ProvideVolumeManager provides the volume manager
func ProvideVolumeManager(cfg *config.Config) volumes.Manager {
	return volumes.NewManager(cfg.DataDir)
}
