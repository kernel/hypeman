package instances

import (
	"fmt"

	"github.com/kernel/hypeman/lib/autostandby"
)

func cloneAutoStandbyPolicy(policy *autostandby.Policy) *autostandby.Policy {
	if policy == nil {
		return nil
	}

	cloned := &autostandby.Policy{
		Enabled:     policy.Enabled,
		IdleTimeout: policy.IdleTimeout,
	}
	if len(policy.IgnoreSourceCIDRs) > 0 {
		cloned.IgnoreSourceCIDRs = append([]string(nil), policy.IgnoreSourceCIDRs...)
	}
	if len(policy.IgnoreDestinationPorts) > 0 {
		cloned.IgnoreDestinationPorts = append([]uint16(nil), policy.IgnoreDestinationPorts...)
	}
	return cloned
}

func normalizeAutoStandbyPolicy(policy *autostandby.Policy) (*autostandby.Policy, error) {
	normalized, err := autostandby.NormalizePolicy(policy)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return normalized, nil
}
