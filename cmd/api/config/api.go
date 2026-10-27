package config

import (
	"fmt"
	"net"
	"slices"
	"strconv"
)

func (c *Config) validateAPIListenPorts() error {
	if len(c.API.ListenPorts) == 0 {
		return nil
	}
	for i, port := range c.API.ListenPorts {
		if port < 1 || port > 65535 {
			return fmt.Errorf("api.listen_ports must be between 1 and 65535, got %d", port)
		}
		if c.API.TLS && port == 80 {
			return fmt.Errorf("api.listen_ports cannot include HTTP port 80 when api.tls is enabled")
		}
		if slices.Contains(c.API.ListenPorts[:i], port) {
			return fmt.Errorf("api.listen_ports contains duplicate port %d", port)
		}
	}
	if c.API.Hostname == "" {
		return nil
	}

	backendPort, err := strconv.Atoi(c.Port)
	if err != nil {
		return fmt.Errorf("api.listen_ports: invalid backend port %q: %w", c.Port, err)
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
		if slices.Contains(c.API.ListenPorts, listener.port) &&
			bindAddressesOverlap(c.Caddy.ListenAddress, listener.address) {
			return fmt.Errorf("api.listen_ports port %d conflicts with %s", listener.port, listener.name)
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
