package autostandby

import (
	"fmt"
	"net/netip"
	"time"
)

// ActiveInboundCount returns the number of active inbound TCP connections for an instance
// and the compiled idle timeout that should be applied to it.
func ActiveInboundCount(inst Instance, conns []Connection) (int, time.Duration, error) {
	compiled, err := compilePolicy(inst.AutoStandby)
	if err != nil {
		return 0, 0, err
	}
	if compiled == nil {
		return 0, 0, nil
	}

	instanceIP, err := netip.ParseAddr(inst.IP)
	if err != nil {
		return 0, 0, fmt.Errorf("parse instance IP %q: %w", inst.IP, err)
	}

	count := 0
	for _, conn := range conns {
		if matchesInboundConnection(instanceIP, compiled, conn) {
			count++
		}
	}

	return count, compiled.idleTimeout, nil
}

func matchesInboundConnection(instanceIP netip.Addr, policy *compiledPolicy, conn Connection) bool {
	if policy == nil {
		return false
	}
	if !conn.TCPState.Active() {
		return false
	}
	if !conn.OriginalDestinationIP.IsValid() || conn.OriginalDestinationIP != instanceIP {
		return false
	}
	if _, ignored := policy.ignorePorts[conn.OriginalDestinationPort]; ignored {
		return false
	}
	if !conn.OriginalSourceIP.IsValid() {
		return false
	}
	for _, prefix := range policy.ignoreSourceCIDRs {
		if prefix.Contains(conn.OriginalSourceIP) {
			return false
		}
	}
	return true
}
