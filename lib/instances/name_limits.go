package instances

import (
	"fmt"
	"regexp"
)

// NamedResourceLimit applies per-instance limits to names matching Pattern.
// The first matching pattern wins, and omitted fields fall back to global limits.
type NamedResourceLimit struct {
	Pattern              string
	re                   *regexp.Regexp
	MaxVcpusPerInstance  *int
	MaxMemoryPerInstance *int64
	MaxOverlaySize       *int64
}

func NewNamedResourceLimit(pattern string, maxVcpus *int, maxMemory *int64, maxOverlay *int64) (NamedResourceLimit, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return NamedResourceLimit{}, fmt.Errorf("compile name limit regex %q: %w", pattern, err)
	}

	return NamedResourceLimit{
		Pattern:              pattern,
		re:                   re,
		MaxVcpusPerInstance:  maxVcpus,
		MaxMemoryPerInstance: maxMemory,
		MaxOverlaySize:       maxOverlay,
	}, nil
}

func (l NamedResourceLimit) matches(name string) bool {
	return l.re != nil && l.re.MatchString(name)
}

func (l ResourceLimits) ForName(name string) ResourceLimits {
	resolved := ResourceLimits{
		MaxOverlaySize:       l.MaxOverlaySize,
		MaxVcpusPerInstance:  l.MaxVcpusPerInstance,
		MaxMemoryPerInstance: l.MaxMemoryPerInstance,
	}

	for _, pattern := range l.NamePatterns {
		if !pattern.matches(name) {
			continue
		}
		if pattern.MaxOverlaySize != nil {
			resolved.MaxOverlaySize = *pattern.MaxOverlaySize
		}
		if pattern.MaxVcpusPerInstance != nil {
			resolved.MaxVcpusPerInstance = *pattern.MaxVcpusPerInstance
		}
		if pattern.MaxMemoryPerInstance != nil {
			resolved.MaxMemoryPerInstance = *pattern.MaxMemoryPerInstance
		}
		break
	}

	return resolved
}

func validateResourceLimitsForName(name string, limits ResourceLimits, overlaySize int64, vcpus int, totalMemory int64) error {
	effective := limits.ForName(name)

	if effective.MaxOverlaySize > 0 && overlaySize > effective.MaxOverlaySize {
		return fmt.Errorf("overlay size %d exceeds maximum allowed size %d", overlaySize, effective.MaxOverlaySize)
	}
	if effective.MaxVcpusPerInstance > 0 && vcpus > effective.MaxVcpusPerInstance {
		return fmt.Errorf("vcpus %d exceeds maximum allowed %d per instance", vcpus, effective.MaxVcpusPerInstance)
	}
	if effective.MaxMemoryPerInstance > 0 && totalMemory > effective.MaxMemoryPerInstance {
		return fmt.Errorf("total memory %d (size + hotplug_size) exceeds maximum allowed %d per instance", totalMemory, effective.MaxMemoryPerInstance)
	}

	return nil
}
