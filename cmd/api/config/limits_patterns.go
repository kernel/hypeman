package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/c2h5oh/datasize"
)

// NamePatternLimitsConfig holds per-name regex resource limit overrides.
// The first matching pattern wins. Omitted fields fall back to the global limits block.
type NamePatternLimitsConfig struct {
	Pattern                  string  `koanf:"pattern"`
	MaxVcpusPerInstance      *int    `koanf:"max_vcpus_per_instance"`
	MaxMemoryPerInstance     *string `koanf:"max_memory_per_instance"`
	MaxOverlaySize           *string `koanf:"max_overlay_size"`
	MaxTotalVcpus            *int    `koanf:"max_total_vcpus"`
	MaxTotalMemory           *string `koanf:"max_total_memory"`
	MaxTotalDisk             *string `koanf:"max_total_disk"`
	MaxTotalNetworkBandwidth *string `koanf:"max_total_network_bandwidth"`
	MaxTotalDiskIO           *string `koanf:"max_total_disk_io"`
}

func validateNamePatternLimits(patterns []NamePatternLimitsConfig) error {
	for i := range patterns {
		cfg := &patterns[i]
		cfg.Pattern = strings.TrimSpace(cfg.Pattern)
		if cfg.Pattern == "" {
			return fmt.Errorf("limits.name_patterns[%d].pattern must not be empty", i)
		}
		if _, err := regexp.Compile(cfg.Pattern); err != nil {
			return fmt.Errorf("limits.name_patterns[%d].pattern must be a valid regex, got %q: %w", i, cfg.Pattern, err)
		}
		if err := validateOptionalNonNegativeInt(fmt.Sprintf("limits.name_patterns[%d].max_vcpus_per_instance", i), cfg.MaxVcpusPerInstance); err != nil {
			return err
		}
		if cfg.MaxMemoryPerInstance != nil {
			value := strings.TrimSpace(*cfg.MaxMemoryPerInstance)
			if value == "" {
				return fmt.Errorf("limits.name_patterns[%d].max_memory_per_instance must not be empty", i)
			}
			*cfg.MaxMemoryPerInstance = value
			if err := validateOptionalByteSize(fmt.Sprintf("limits.name_patterns[%d].max_memory_per_instance", i), value); err != nil {
				return err
			}
		}
		if cfg.MaxOverlaySize != nil {
			value := strings.TrimSpace(*cfg.MaxOverlaySize)
			if value == "" {
				return fmt.Errorf("limits.name_patterns[%d].max_overlay_size must not be empty", i)
			}
			*cfg.MaxOverlaySize = value
			if err := validateOptionalByteSize(fmt.Sprintf("limits.name_patterns[%d].max_overlay_size", i), value); err != nil {
				return err
			}
		}
		if err := validateOptionalNonNegativeInt(fmt.Sprintf("limits.name_patterns[%d].max_total_vcpus", i), cfg.MaxTotalVcpus); err != nil {
			return err
		}
		if err := validateOptionalByteSizePtr(fmt.Sprintf("limits.name_patterns[%d].max_total_memory", i), cfg.MaxTotalMemory); err != nil {
			return err
		}
		if err := validateOptionalByteSizePtr(fmt.Sprintf("limits.name_patterns[%d].max_total_disk", i), cfg.MaxTotalDisk); err != nil {
			return err
		}
		if err := validateOptionalBandwidthPtr(fmt.Sprintf("limits.name_patterns[%d].max_total_network_bandwidth", i), cfg.MaxTotalNetworkBandwidth); err != nil {
			return err
		}
		if err := validateOptionalBandwidthPtr(fmt.Sprintf("limits.name_patterns[%d].max_total_disk_io", i), cfg.MaxTotalDiskIO); err != nil {
			return err
		}
	}
	return nil
}

func validateOptionalNonNegativeInt(field string, value *int) error {
	if value == nil {
		return nil
	}
	if *value < 0 {
		return fmt.Errorf("%s must be >= 0, got %d", field, *value)
	}
	return nil
}

func validateOptionalByteSizePtr(field string, value *string) error {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	*value = trimmed
	return validateOptionalByteSize(field, trimmed)
}

func validateOptionalBandwidthPtr(field string, value *string) error {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	*value = trimmed
	if _, err := parseBandwidth(trimmed); err != nil {
		return fmt.Errorf("%s must be a valid bandwidth, got %q: %w", field, trimmed, err)
	}
	return nil
}

func parseBandwidth(limit string) (int64, error) {
	limit = strings.TrimSpace(strings.ToLower(limit))

	if strings.HasSuffix(limit, "bps") {
		numPart := strings.TrimSpace(strings.TrimSuffix(limit, "bps"))

		var multiplier int64 = 1
		switch {
		case strings.HasSuffix(numPart, "g"):
			multiplier = 1000 * 1000 * 1000
			numPart = strings.TrimSuffix(numPart, "g")
		case strings.HasSuffix(numPart, "m"):
			multiplier = 1000 * 1000
			numPart = strings.TrimSuffix(numPart, "m")
		case strings.HasSuffix(numPart, "k"):
			multiplier = 1000
			numPart = strings.TrimSuffix(numPart, "k")
		}

		bits, err := strconv.ParseInt(strings.TrimSpace(numPart), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid number: %s", numPart)
		}
		return (bits * multiplier) / 8, nil
	}

	limit = strings.TrimSuffix(limit, "/s")
	limit = strings.TrimSuffix(limit, "ps")

	var size datasize.ByteSize
	if err := size.UnmarshalText([]byte(limit)); err != nil {
		return 0, fmt.Errorf("parse as bytes: %w", err)
	}

	return int64(size), nil
}
