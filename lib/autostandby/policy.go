package autostandby

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// NormalizePolicy validates and canonicalizes a policy for storage.
func NormalizePolicy(policy *Policy) (*Policy, error) {
	if policy == nil {
		return nil, nil
	}

	if !policy.Enabled {
		return &Policy{Enabled: false}, nil
	}

	idleTimeout := strings.TrimSpace(policy.IdleTimeout)
	if idleTimeout == "" {
		return nil, fmt.Errorf("auto_standby.idle_timeout is required when enabled")
	}

	parsedTimeout, err := time.ParseDuration(idleTimeout)
	if err != nil {
		return nil, fmt.Errorf("auto_standby.idle_timeout must be a valid duration: %w", err)
	}
	if parsedTimeout <= 0 {
		return nil, fmt.Errorf("auto_standby.idle_timeout must be positive")
	}

	normalized := &Policy{
		Enabled:     true,
		IdleTimeout: parsedTimeout.String(),
	}

	if len(policy.IgnoreSourceCIDRs) > 0 {
		seen := make(map[string]struct{}, len(policy.IgnoreSourceCIDRs))
		for _, raw := range policy.IgnoreSourceCIDRs {
			cidr := strings.TrimSpace(raw)
			if cidr == "" {
				continue
			}
			prefix, err := netip.ParsePrefix(cidr)
			if err != nil {
				return nil, fmt.Errorf("auto_standby.ignore_source_cidrs contains invalid CIDR %q: %w", cidr, err)
			}
			canonical := prefix.Masked().String()
			if _, ok := seen[canonical]; ok {
				continue
			}
			seen[canonical] = struct{}{}
			normalized.IgnoreSourceCIDRs = append(normalized.IgnoreSourceCIDRs, canonical)
		}
		slices.Sort(normalized.IgnoreSourceCIDRs)
	}

	if len(policy.IgnoreDestinationPorts) > 0 {
		seen := make(map[uint16]struct{}, len(policy.IgnoreDestinationPorts))
		for _, port := range policy.IgnoreDestinationPorts {
			if port == 0 {
				return nil, fmt.Errorf("auto_standby.ignore_destination_ports must not contain 0")
			}
			if _, ok := seen[port]; ok {
				continue
			}
			seen[port] = struct{}{}
			normalized.IgnoreDestinationPorts = append(normalized.IgnoreDestinationPorts, port)
		}
		slices.Sort(normalized.IgnoreDestinationPorts)
	}

	return normalized, nil
}

func compilePolicy(policy *Policy) (*compiledPolicy, error) {
	normalized, err := NormalizePolicy(policy)
	if err != nil {
		return nil, err
	}
	if normalized == nil || !normalized.Enabled {
		return nil, nil
	}

	idleTimeout, err := time.ParseDuration(normalized.IdleTimeout)
	if err != nil {
		return nil, fmt.Errorf("parse normalized auto-standby idle timeout: %w", err)
	}

	compiled := &compiledPolicy{
		idleTimeout: idleTimeout,
		ignorePorts: make(map[uint16]struct{}, len(normalized.IgnoreDestinationPorts)),
	}

	for _, raw := range normalized.IgnoreSourceCIDRs {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return nil, fmt.Errorf("parse normalized auto-standby source CIDR %q: %w", raw, err)
		}
		compiled.ignoreSourceCIDRs = append(compiled.ignoreSourceCIDRs, prefix)
	}
	for _, port := range normalized.IgnoreDestinationPorts {
		compiled.ignorePorts[port] = struct{}{}
	}

	return compiled, nil
}
