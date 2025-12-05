package providers

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/c2h5oh/datasize"
	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/ingress"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/network"
	hypemanotel "github.com/onkernel/hypeman/lib/otel"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/registry"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/onkernel/hypeman/lib/volumes"
	"go.opentelemetry.io/otel"
)

// ProvideLogger provides a structured logger with subsystem-specific levels
func ProvideLogger() *slog.Logger {
	cfg := logger.NewConfig()
	otelHandler := hypemanotel.GetGlobalLogHandler()
	return logger.NewSubsystemLogger(logger.SubsystemAPI, cfg, otelHandler)
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
	meter := otel.GetMeterProvider().Meter("hypeman")
	return images.NewManager(p, cfg.MaxConcurrentBuilds, meter)
}

// ProvideSystemManager provides the system manager
func ProvideSystemManager(p *paths.Paths) system.Manager {
	return system.NewManager(p)
}

// ProvideNetworkManager provides the network manager
func ProvideNetworkManager(p *paths.Paths, cfg *config.Config) network.Manager {
	meter := otel.GetMeterProvider().Meter("hypeman")
	return network.NewManager(p, cfg, meter)
}

// ProvideInstanceManager provides the instance manager
func ProvideInstanceManager(p *paths.Paths, cfg *config.Config, imageManager images.Manager, systemManager system.Manager, networkManager network.Manager, volumeManager volumes.Manager) (instances.Manager, error) {
	// Parse max overlay size from config
	var maxOverlaySize datasize.ByteSize
	if err := maxOverlaySize.UnmarshalText([]byte(cfg.MaxOverlaySize)); err != nil {
		return nil, fmt.Errorf("failed to parse MAX_OVERLAY_SIZE '%s': %w (expected format like '100GB', '50G', '10GiB')", cfg.MaxOverlaySize, err)
	}

	// Parse max memory per instance (empty or "0" means unlimited)
	var maxMemoryPerInstance int64
	if cfg.MaxMemoryPerInstance != "" && cfg.MaxMemoryPerInstance != "0" {
		var memSize datasize.ByteSize
		if err := memSize.UnmarshalText([]byte(cfg.MaxMemoryPerInstance)); err != nil {
			return nil, fmt.Errorf("failed to parse MAX_MEMORY_PER_INSTANCE '%s': %w", cfg.MaxMemoryPerInstance, err)
		}
		maxMemoryPerInstance = int64(memSize)
	}

	// Parse max total memory (empty or "0" means unlimited)
	var maxTotalMemory int64
	if cfg.MaxTotalMemory != "" && cfg.MaxTotalMemory != "0" {
		var memSize datasize.ByteSize
		if err := memSize.UnmarshalText([]byte(cfg.MaxTotalMemory)); err != nil {
			return nil, fmt.Errorf("failed to parse MAX_TOTAL_MEMORY '%s': %w", cfg.MaxTotalMemory, err)
		}
		maxTotalMemory = int64(memSize)
	}

	limits := instances.ResourceLimits{
		MaxOverlaySize:       int64(maxOverlaySize),
		MaxVcpusPerInstance:  cfg.MaxVcpusPerInstance,
		MaxMemoryPerInstance: maxMemoryPerInstance,
		MaxTotalVcpus:        cfg.MaxTotalVcpus,
		MaxTotalMemory:       maxTotalMemory,
	}

	meter := otel.GetMeterProvider().Meter("hypeman")
	tracer := otel.GetTracerProvider().Tracer("hypeman")
	return instances.NewManager(p, imageManager, systemManager, networkManager, volumeManager, limits, meter, tracer), nil
}

// ProvideVolumeManager provides the volume manager
func ProvideVolumeManager(p *paths.Paths, cfg *config.Config) (volumes.Manager, error) {
	// Parse max total volume storage (empty or "0" means unlimited)
	var maxTotalVolumeStorage int64
	if cfg.MaxTotalVolumeStorage != "" && cfg.MaxTotalVolumeStorage != "0" {
		var storageSize datasize.ByteSize
		if err := storageSize.UnmarshalText([]byte(cfg.MaxTotalVolumeStorage)); err != nil {
			return nil, fmt.Errorf("failed to parse MAX_TOTAL_VOLUME_STORAGE '%s': %w", cfg.MaxTotalVolumeStorage, err)
		}
		maxTotalVolumeStorage = int64(storageSize)
	}

	meter := otel.GetMeterProvider().Meter("hypeman")
	return volumes.NewManager(p, maxTotalVolumeStorage, meter), nil
}

// ProvideRegistry provides the OCI registry for image push
func ProvideRegistry(p *paths.Paths, imageManager images.Manager) (*registry.Registry, error) {
	return registry.New(p, imageManager)
}

// ProvideIngressManager provides the ingress manager
func ProvideIngressManager(p *paths.Paths, cfg *config.Config, instanceManager instances.Manager) ingress.Manager {
	ingressConfig := ingress.Config{
		ListenAddress:  cfg.EnvoyListenAddress,
		AdminAddress:   cfg.EnvoyAdminAddress,
		AdminPort:      cfg.EnvoyAdminPort,
		StopOnShutdown: cfg.EnvoyStopOnShutdown,
		OTEL: ingress.OTELConfig{
			Enabled:           cfg.OtelEnabled,
			Endpoint:          cfg.OtelEndpoint,
			ServiceName:       cfg.OtelServiceName + "-envoy",
			ServiceInstanceID: cfg.OtelServiceInstanceID,
			Insecure:          cfg.OtelInsecure,
			Environment:       cfg.Env,
		},
	}

	// IngressResolver from instances package implements ingress.InstanceResolver
	resolver := instances.NewIngressResolver(instanceManager)
	return ingress.NewManager(p, ingressConfig, resolver)
}
