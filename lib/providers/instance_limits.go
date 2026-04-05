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
		pattern, err := parseNamedResourceLimit(i, patternCfg)
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

func parseNamedResourceLimit(i int, cfg config.NamePatternLimitsConfig) (instances.NamedResourceLimit, error) {
	parsed := instances.NamedResourceLimitConfig{
		MaxVcpusPerInstance: parseOptionalIntPtr(cfg.MaxVcpusPerInstance),
		MaxTotalVcpus:       parseOptionalIntPtr(cfg.MaxTotalVcpus),
	}

	byteFields := []struct {
		field string
		src   *string
		dst   **int64
	}{
		{field: "max_memory_per_instance", src: cfg.MaxMemoryPerInstance, dst: &parsed.MaxMemoryPerInstance},
		{field: "max_overlay_size", src: cfg.MaxOverlaySize, dst: &parsed.MaxOverlaySize},
		{field: "max_total_memory", src: cfg.MaxTotalMemory, dst: &parsed.MaxTotalMemory},
		{field: "max_total_disk", src: cfg.MaxTotalDisk, dst: &parsed.MaxTotalDisk},
	}
	for _, field := range byteFields {
		value, err := parseOptionalByteSizePtr(fmt.Sprintf("limits.name_patterns[%d].%s", i, field.field), field.src)
		if err != nil {
			return instances.NamedResourceLimit{}, err
		}
		*field.dst = value
	}

	bandwidthFields := []struct {
		field string
		src   *string
		dst   **int64
	}{
		{field: "max_total_network_bandwidth", src: cfg.MaxTotalNetworkBandwidth, dst: &parsed.MaxTotalNetworkBandwidth},
		{field: "max_total_disk_io", src: cfg.MaxTotalDiskIO, dst: &parsed.MaxTotalDiskIO},
	}
	for _, field := range bandwidthFields {
		value, err := parseOptionalBandwidthPtr(fmt.Sprintf("limits.name_patterns[%d].%s", i, field.field), field.src)
		if err != nil {
			return instances.NamedResourceLimit{}, err
		}
		*field.dst = value
	}

	return instances.NewNamedResourceLimit(cfg.Pattern, parsed)
}

func parseOptionalIntPtr(value *int) *int {
	if value == nil {
		return nil
	}
	parsed := *value
	return &parsed
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
