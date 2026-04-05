package config

import (
	"fmt"
	"regexp"
	"strings"
)

// NamePatternLimitsConfig holds per-name regex resource limit overrides.
// The first matching pattern wins. Omitted fields fall back to the global limits block.
type NamePatternLimitsConfig struct {
	Pattern              string  `koanf:"pattern"`
	MaxVcpusPerInstance  *int    `koanf:"max_vcpus_per_instance"`
	MaxMemoryPerInstance *string `koanf:"max_memory_per_instance"`
	MaxOverlaySize       *string `koanf:"max_overlay_size"`
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
		if cfg.MaxVcpusPerInstance != nil && *cfg.MaxVcpusPerInstance < 0 {
			return fmt.Errorf("limits.name_patterns[%d].max_vcpus_per_instance must be >= 0, got %d", i, *cfg.MaxVcpusPerInstance)
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
	}
	return nil
}
