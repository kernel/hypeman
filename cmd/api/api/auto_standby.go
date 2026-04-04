package api

import (
	"fmt"

	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/samber/lo"
)

func toDomainAutoStandbyPolicy(policy *oapi.AutoStandbyPolicy) (*autostandby.Policy, error) {
	if policy == nil {
		return nil, nil
	}

	out := &autostandby.Policy{}
	if policy.Enabled != nil {
		out.Enabled = *policy.Enabled
	}
	if policy.IdleTimeout != nil {
		out.IdleTimeout = *policy.IdleTimeout
	}
	if policy.IgnoreSourceCidrs != nil {
		out.IgnoreSourceCIDRs = append([]string(nil), (*policy.IgnoreSourceCidrs)...)
	}
	if policy.IgnoreDestinationPorts != nil {
		out.IgnoreDestinationPorts = make([]uint16, 0, len(*policy.IgnoreDestinationPorts))
		for _, port := range *policy.IgnoreDestinationPorts {
			if port < 0 || port > 65535 {
				return nil, fmt.Errorf("auto_standby.ignore_destination_ports must be between 1 and 65535")
			}
			out.IgnoreDestinationPorts = append(out.IgnoreDestinationPorts, uint16(port))
		}
	}

	return out, nil
}

func toOAPIAutoStandbyPolicy(policy *autostandby.Policy) *oapi.AutoStandbyPolicy {
	if policy == nil {
		return nil
	}

	out := &oapi.AutoStandbyPolicy{
		Enabled: lo.ToPtr(policy.Enabled),
	}
	if policy.IdleTimeout != "" {
		out.IdleTimeout = lo.ToPtr(policy.IdleTimeout)
	}
	if len(policy.IgnoreSourceCIDRs) > 0 {
		out.IgnoreSourceCidrs = lo.ToPtr(append([]string(nil), policy.IgnoreSourceCIDRs...))
	}
	if len(policy.IgnoreDestinationPorts) > 0 {
		ports := make([]int, 0, len(policy.IgnoreDestinationPorts))
		for _, port := range policy.IgnoreDestinationPorts {
			ports = append(ports, int(port))
		}
		out.IgnoreDestinationPorts = &ports
	}

	return out
}
