package providers

import (
	"fmt"
	"strings"

	"github.com/c2h5oh/datasize"
	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/resources"
)

func parseInstanceLimits(cfg *config.Config) (instances.ResourceLimits, error) {
	maxOverlaySize, err := parseByteSizeLimit("limits.max_overlay_size", cfg.Limits.MaxOverlaySize)
	if err != nil {
		return instances.ResourceLimits{}, err
	}

	maxMemoryPerInstance, err := parseOptionalByteSizeLimit("limits.max_memory_per_instance", cfg.Limits.MaxMemoryPerInstance)
	if err != nil {
		return instances.ResourceLimits{}, err
	}

	namePatterns := make([]instances.NamedResourceLimit, 0, len(cfg.Limits.NamePatterns))
	for i, patternCfg := range cfg.Limits.NamePatterns {
		var maxVcpus *int
		if patternCfg.MaxVcpusPerInstance != nil {
			value := *patternCfg.MaxVcpusPerInstance
			maxVcpus = &value
		}
		var maxTotalVcpus *int
		if patternCfg.MaxTotalVcpus != nil {
			value := *patternCfg.MaxTotalVcpus
			maxTotalVcpus = &value
		}

		maxMemory, err := parseOptionalByteSizePtr(fmt.Sprintf("limits.name_patterns[%d].max_memory_per_instance", i), patternCfg.MaxMemoryPerInstance)
		if err != nil {
			return instances.ResourceLimits{}, err
		}
		maxOverlay, err := parseOptionalByteSizePtr(fmt.Sprintf("limits.name_patterns[%d].max_overlay_size", i), patternCfg.MaxOverlaySize)
		if err != nil {
			return instances.ResourceLimits{}, err
		}
		maxTotalMemory, err := parseOptionalByteSizePtr(fmt.Sprintf("limits.name_patterns[%d].max_total_memory", i), patternCfg.MaxTotalMemory)
		if err != nil {
			return instances.ResourceLimits{}, err
		}
		maxTotalDisk, err := parseOptionalByteSizePtr(fmt.Sprintf("limits.name_patterns[%d].max_total_disk", i), patternCfg.MaxTotalDisk)
		if err != nil {
			return instances.ResourceLimits{}, err
		}
		maxTotalNetwork, err := parseOptionalBandwidthPtr(fmt.Sprintf("limits.name_patterns[%d].max_total_network_bandwidth", i), patternCfg.MaxTotalNetworkBandwidth)
		if err != nil {
			return instances.ResourceLimits{}, err
		}
		maxTotalDiskIO, err := parseOptionalBandwidthPtr(fmt.Sprintf("limits.name_patterns[%d].max_total_disk_io", i), patternCfg.MaxTotalDiskIO)
		if err != nil {
			return instances.ResourceLimits{}, err
		}

		pattern, err := instances.NewNamedResourceLimit(patternCfg.Pattern, instances.NamedResourceLimitConfig{
			MaxVcpusPerInstance:      maxVcpus,
			MaxMemoryPerInstance:     maxMemory,
			MaxOverlaySize:           maxOverlay,
			MaxTotalVcpus:            maxTotalVcpus,
			MaxTotalMemory:           maxTotalMemory,
			MaxTotalDisk:             maxTotalDisk,
			MaxTotalNetworkBandwidth: maxTotalNetwork,
			MaxTotalDiskIO:           maxTotalDiskIO,
		})
		if err != nil {
			return instances.ResourceLimits{}, fmt.Errorf("parse limits.name_patterns[%d]: %w", i, err)
		}
		namePatterns = append(namePatterns, pattern)
	}

	return instances.ResourceLimits{
		MaxOverlaySize:       maxOverlaySize,
		MaxVcpusPerInstance:  cfg.Limits.MaxVcpusPerInstance,
		MaxMemoryPerInstance: maxMemoryPerInstance,
		NamePatterns:         namePatterns,
	}, nil
}

func parseOptionalByteSizePtr(field string, value *string) (*int64, error) {
	if value == nil {
		return nil, nil
	}

	parsed, err := parseOptionalByteSizeLimit(field, *value)
	if err != nil {
		return nil, err
	}

	return &parsed, nil
}

func parseOptionalBandwidthPtr(field string, value *string) (*int64, error) {
	if value == nil {
		return nil, nil
	}

	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil, nil
	}

	parsed, err := resources.ParseBandwidth(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%s must be a valid bandwidth, got %q: %w", field, trimmed, err)
	}

	return &parsed, nil
}

func parseOptionalByteSizeLimit(field string, value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}

	return parseByteSizeLimit(field, value)
}

func parseByteSizeLimit(field string, value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("%s must not be empty", field)
	}

	var size datasize.ByteSize
	if err := size.UnmarshalText([]byte(value)); err != nil {
		return 0, fmt.Errorf("%s must be a valid byte size, got %q: %w", field, value, err)
	}

	return int64(size), nil
}
