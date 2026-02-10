package network

import (
	"fmt"
	"net"
)

// DeriveGateway returns the first usable IP in a subnet (used as gateway).
// e.g., 10.100.0.0/16 -> 10.100.0.1
func DeriveGateway(cidr string) (string, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse CIDR: %w", err)
	}

	// Gateway is network address + 1
	gateway := make(net.IP, len(ipNet.IP))
	copy(gateway, ipNet.IP)
	gateway[len(gateway)-1]++ // Increment last octet

	return gateway.String(), nil
}
