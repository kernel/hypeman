package config

import (
	"fmt"
	"net"
	"slices"
	"strconv"
)

func (c *Config) validateAPIHTTPSPorts() error {
	if len(c.API.HTTPSPorts) == 0 {
		return nil
	}
	if !c.API.TLS {
		return fmt.Errorf("api.https_ports requires api.tls to be enabled")
	}
	seen := make(map[int]bool)
	for _, port := range c.API.HTTPSPorts {
		if port < 1 || port > 65535 || port == 80 {
			return fmt.Errorf("api.https_ports must be between 1 and 65535, excluding HTTP port 80, got %d", port)
		}
		if seen[port] {
			return fmt.Errorf("api.https_ports contains duplicate port %d", port)
		}
		seen[port] = true
	}
	if c.API.Hostname == "" {
		return nil
	}

	backendPort, err := strconv.Atoi(c.Port)
	if err != nil {
		return fmt.Errorf("api.https_ports: invalid backend port %q: %w", c.Port, err)
	}
	for _, listener := range []struct {
		name    string
		address string
		port    int
	}{
		{"backend port", c.ListenAddress, backendPort},
		{"metrics.port", c.Metrics.ListenAddress, c.Metrics.Port},
		{"caddy.admin_port", c.Caddy.AdminAddress, c.Caddy.AdminPort},
	} {
		if slices.Contains(c.API.HTTPSPorts, listener.port) &&
			bindAddressesOverlap(c.Caddy.ListenAddress, listener.address) {
			return fmt.Errorf("api.https_ports port %d conflicts with %s", listener.port, listener.name)
		}
	}
	return nil
}

func bindAddressesOverlap(a, b string) bool {
	if a == "" || b == "" || a == b {
		return true
	}
	ipA, ipB := net.ParseIP(a), net.ParseIP(b)
	// Hostnames may resolve to the same address; only allow provably disjoint IPs.
	if ipA == nil || ipB == nil {
		return true
	}
	return ipA.IsUnspecified() || ipB.IsUnspecified() || ipA.Equal(ipB)
}
