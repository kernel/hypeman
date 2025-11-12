package network

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"strings"

	"github.com/onkernel/hypeman/lib/logger"
)

// AllocateNetwork allocates IP/MAC/TAP for instance
func (m *manager) AllocateNetwork(ctx context.Context, req AllocateRequest) (*NetworkConfig, error) {
	log := logger.FromContext(ctx)

	// 1. If no network requested, return nil (no network)
	if req.Network == "" {
		return nil, nil
	}

	// 2. Validate network exists
	network, err := m.GetNetwork(ctx, req.Network)
	if err != nil {
		return nil, err
	}

	// 3. Check name uniqueness in network
	exists, err := m.NameExistsInNetwork(ctx, req.InstanceName, req.Network)
	if err != nil {
		return nil, fmt.Errorf("check name exists: %w", err)
	}
	if exists {
		return nil, fmt.Errorf("%w: instance name '%s' already exists in network '%s'",
			ErrNameExists, req.InstanceName, req.Network)
	}

	// 4. Allocate next available IP
	// TODO @sjmiller609 review: does random IP decrease probability of conflict in case of moving standby VMs across hosts?
	ip, err := m.allocateNextIP(ctx, req.Network, network.Subnet)
	if err != nil {
		return nil, fmt.Errorf("allocate IP: %w", err)
	}

	// 5. Generate MAC (02:00:00:... format - locally administered)
	mac, err := generateMAC()
	if err != nil {
		return nil, fmt.Errorf("generate MAC: %w", err)
	}

	// 6. Generate TAP name (tap-{first8chars-of-id})
	tap := generateTAPName(req.InstanceID)

	// 7. Create TAP device
	if err := m.createTAPDevice(tap, network.Bridge, network.Isolated); err != nil {
		return nil, fmt.Errorf("create TAP device: %w", err)
	}

	// 8. Register DNS
	if err := m.reloadDNS(ctx); err != nil {
		// Cleanup TAP on DNS failure
		m.deleteTAPDevice(tap)
		return nil, fmt.Errorf("register DNS: %w", err)
	}

	log.InfoContext(ctx, "allocated network",
		"instance_id", req.InstanceID,
		"instance_name", req.InstanceName,
		"network", req.Network,
		"ip", ip,
		"mac", mac,
		"tap", tap)

	// 9. Calculate netmask from subnet
	_, ipNet, _ := net.ParseCIDR(network.Subnet)
	netmask := fmt.Sprintf("%d.%d.%d.%d", ipNet.Mask[0], ipNet.Mask[1], ipNet.Mask[2], ipNet.Mask[3])

	// 10. Return config (will be used in CH VmConfig)
	return &NetworkConfig{
		IP:        ip,
		MAC:       mac,
		Gateway:   network.Gateway,
		Netmask:   netmask,
		DNS:       network.Gateway, // dnsmasq listens on gateway
		TAPDevice: tap,
	}, nil
}

// RecreateNetwork recreates TAP for restore from standby
func (m *manager) RecreateNetwork(ctx context.Context, instanceID string) error {
	log := logger.FromContext(ctx)

	// 1. Derive allocation from snapshot
	alloc, err := m.deriveAllocation(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("derive allocation: %w", err)
	}
	if alloc == nil {
		// No network configured for this instance
		return nil
	}

	// 2. Get network details
	network, err := m.GetNetwork(ctx, alloc.Network)
	if err != nil {
		return fmt.Errorf("get network: %w", err)
	}

	// 3. Recreate TAP device with same name
	if err := m.createTAPDevice(alloc.TAPDevice, network.Bridge, network.Isolated); err != nil {
		return fmt.Errorf("create TAP device: %w", err)
	}

	log.InfoContext(ctx, "recreated network for restore",
		"instance_id", instanceID,
		"network", alloc.Network,
		"tap", alloc.TAPDevice)

	return nil
}

// ReleaseNetwork cleans up network allocation (shutdown/delete)
func (m *manager) ReleaseNetwork(ctx context.Context, instanceID string) error {
	log := logger.FromContext(ctx)

	// 1. Derive current allocation
	alloc, err := m.deriveAllocation(ctx, instanceID)
	if err != nil || alloc == nil {
		// No network or already released
		return nil
	}

	// 2. Delete TAP device (best effort)
	// TODO @sjmiller609 review: possibility / how to address straggler TAP devices, e.g. host power loss what happens
	if err := m.deleteTAPDevice(alloc.TAPDevice); err != nil {
		log.WarnContext(ctx, "failed to delete TAP device", "tap", alloc.TAPDevice, "error", err)
	}

	// 3. Reload DNS (removes entries)
	if err := m.reloadDNS(ctx); err != nil {
		log.WarnContext(ctx, "failed to reload DNS", "error", err)
	}

	log.InfoContext(ctx, "released network",
		"instance_id", instanceID,
		"network", alloc.Network,
		"ip", alloc.IP)

	return nil
}

// allocateNextIP finds the next available IP in the subnet
func (m *manager) allocateNextIP(ctx context.Context, networkName, subnet string) (string, error) {
	// Parse subnet
	ip, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return "", fmt.Errorf("parse subnet: %w", err)
	}

	// Get all currently allocated IPs in this network
	allocations, err := m.ListAllocations(ctx)
	if err != nil {
		return "", fmt.Errorf("list allocations: %w", err)
	}

	usedIPs := make(map[string]bool)
	for _, alloc := range allocations {
		if alloc.Network == networkName {
			usedIPs[alloc.IP] = true
		}
	}

	// Reserve gateway IP
	usedIPs[ip.String()] = true

	// Iterate through subnet to find free IP
	// Start from .10 (reserve .1-.9 for infrastructure)
	for ip := incrementIP(ip, 10); ipNet.Contains(ip); ip = incrementIP(ip, 1) {
		ipStr := ip.String()

		// Skip broadcast address
		if ip[len(ip)-1] == 255 {
			continue
		}

		if !usedIPs[ipStr] {
			return ipStr, nil
		}
	}

	return "", fmt.Errorf("no available IPs in subnet %s", subnet)
}

// incrementIP increments IP address by n
func incrementIP(ip net.IP, n int) net.IP {
	// Ensure we're working with IPv4 (4 bytes)
	ip4 := ip.To4()
	if ip4 == nil {
		// Should not happen with our subnet parsing, but handle it
		return ip
	}

	result := make(net.IP, 4)
	copy(result, ip4)

	// Convert to 32-bit integer, increment, convert back
	val := uint32(result[0])<<24 | uint32(result[1])<<16 | uint32(result[2])<<8 | uint32(result[3])
	val += uint32(n)
	result[0] = byte(val >> 24)
	result[1] = byte(val >> 16)
	result[2] = byte(val >> 8)
	result[3] = byte(val)

	return result
}

// generateMAC generates a random MAC address with local administration bit set
func generateMAC() (string, error) {
	// Generate 6 random bytes
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	// Set local administration bit (bit 1 of first byte)
	// Use 02:00:00:... format (locally administered, unicast)
	buf[0] = 0x02
	buf[1] = 0x00
	buf[2] = 0x00

	// Format as MAC address
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		buf[0], buf[1], buf[2], buf[3], buf[4], buf[5]), nil
}

// generateTAPName generates TAP device name from instance ID
func generateTAPName(instanceID string) string {
	// Use first 8 chars of instance ID
	// tap-{8chars} fits within 15-char Linux interface name limit
	shortID := instanceID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	return "tap-" + strings.ToLower(shortID)
}

