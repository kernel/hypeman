package egressproxy

import (
	"context"
	"fmt"
	"net"
	"net/netip"
)

type ipResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

var blockedEgressPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
}

func publicDialContext(resolver ipResolver, dial dialContextFunc) dialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses := make([]netip.Addr, 0, 1)
		if parsed, parseErr := netip.ParseAddr(host); parseErr == nil {
			addresses = append(addresses, parsed)
		} else {
			addresses, err = resolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
		}
		if len(addresses) == 0 {
			return nil, fmt.Errorf("egress destination did not resolve")
		}
		selected := addresses[0]
		for _, candidate := range addresses {
			if !isPublicEgressIP(candidate) {
				return nil, fmt.Errorf("egress destination resolves to a blocked address")
			}
			if candidate.Is4() {
				selected = candidate
			}
		}
		return dial(ctx, network, net.JoinHostPort(selected.String(), port))
	}
}

func isPublicEgressIP(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	for _, prefix := range blockedEgressPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}
