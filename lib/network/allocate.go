package network

import (
	"context"
	"crypto/rand"
	"fmt"
	mathrand "math/rand"
	"net"
	"strings"

	"github.com/onkernel/hypeman/lib/logger"
)

// CreateAllocation allocates IP/MAC/TAP for instance on the default network
func (m *manager) CreateAllocation(ctx context.Context, req AllocateRequest) (*NetworkConfig, error) {
	// Acquire lock to prevent concurrent allocations from:
	// 1. Picking the same IP address
	// 2. Creating duplicate instance names
	m.mu.Lock()
	defer m.mu.Unlock()

	log := logger.FromContext(ctx)

	// 1. Get default network
	network, err := m.getDefaultNetwork(ctx)
	if err != nil {
		return nil, fmt.Errorf("get default network: %w", err)
	}

	// 2. Check name uniqueness
	exists, err := m.NameExists(ctx, req.InstanceName)
	if err != nil {
		return nil, fmt.Errorf("check name exists: %w", err)
	}
	if exists {
		return nil, fmt.Errorf("%w: instance name '%s' already exists, can't assign into same network: %s",
			ErrNameExists, req.InstanceName, network.Name)
	}

	// 3. Allocate random available IP
	// Random selection reduces predictability and helps distribute IPs across the subnet.
	// This is especially useful for large /16 networks and reduces conflicts when
	// moving standby VMs across hosts.
	ip, err := m.allocateNextIP(ctx, network.Subnet)
	if err != nil {
		return nil, fmt.Errorf("allocate IP: %w", err)
	}

	// 4. Generate MAC (02:00:00:... format - locally administered)
	mac, err := generateMAC()
	if err != nil {
		return nil, fmt.Errorf("generate MAC: %w", err)
	}

	// 5. Generate TAP name (tap-{first8chars-of-id})
	tap := generateTAPName(req.InstanceID)

	// 6. Create TAP device
	if err := m.createTAPDevice(tap, network.Bridge, network.Isolated); err != nil {
		return nil, fmt.Errorf("create TAP device: %w", err)
	}
	m.recordTAPOperation(ctx, "create")

	log.InfoContext(ctx, "allocated network",
		"instance_id", req.InstanceID,
		"instance_name", req.InstanceName,
		"network", "default",
		"ip", ip,
		"mac", mac,
		"tap", tap)

	// 7. Calculate netmask from subnet
	_, ipNet, _ := net.ParseCIDR(network.Subnet)
	netmask := fmt.Sprintf("%d.%d.%d.%d", ipNet.Mask[0], ipNet.Mask[1], ipNet.Mask[2], ipNet.Mask[3])

	// 8. Return config (will be used in CH VmConfig)
	return &NetworkConfig{
		IP:        ip,
		MAC:       mac,
		Gateway:   network.Gateway,
		Netmask:   netmask,
		DNS:       m.config.DNSServer,
		TAPDevice: tap,
	}, nil
}

// RecreateAllocation recreates TAP for restore from standby
// Note: No lock needed - this operation:
// 1. Doesn't allocate new IPs (reuses existing from snapshot)
// 2. Is already protected by instance-level locking
// 3. Uses deterministic TAP names that can't conflict
func (m *manager) RecreateAllocation(ctx context.Context, instanceID string) error {
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

	// 2. Get default network details
	network, err := m.getDefaultNetwork(ctx)
	if err != nil {
		return fmt.Errorf("get default network: %w", err)
	}

	// 3. Recreate TAP device with same name
	if err := m.createTAPDevice(alloc.TAPDevice, network.Bridge, network.Isolated); err != nil {
		return fmt.Errorf("create TAP device: %w", err)
	}
	m.recordTAPOperation(ctx, "create")

	log.InfoContext(ctx, "recreated network for restore",
		"instance_id", instanceID,
		"network", "default",
		"tap", alloc.TAPDevice)

	return nil
}

// ReleaseAllocation cleans up network allocation (shutdown/delete)
// Takes the allocation directly since it should be retrieved before the VMM is killed.
// If alloc is nil, this is a no-op (network not allocated or already released).
// Note: TAP devices created with explicit Owner/Group fields do NOT auto-delete when
// the process closes the file descriptor. They persist in the kernel and must be
// explicitly deleted via this function. In case of unexpected scenarios like host
// power loss, straggler TAP devices may remain until the host is rebooted or manually cleaned up.
func (m *manager) ReleaseAllocation(ctx context.Context, alloc *Allocation) error {
	log := logger.FromContext(ctx)

	// If no allocation provided, nothing to clean up
	if alloc == nil {
		return nil
	}

	// 1. Delete TAP device (best effort)
	if err := m.deleteTAPDevice(alloc.TAPDevice); err != nil {
		log.WarnContext(ctx, "failed to delete TAP device", "tap", alloc.TAPDevice, "error", err)
	} else {
		m.recordTAPOperation(ctx, "delete")
	}

	log.InfoContext(ctx, "released network",
		"instance_id", alloc.InstanceID,
		"network", alloc.Network,
		"ip", alloc.IP)

	return nil
}

// allocateNextIP picks a random available IP in the subnet
// Retries up to 5 times if conflicts occur
func (m *manager) allocateNextIP(ctx context.Context, subnet string) (string, error) {
	// Parse subnet
	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return "", fmt.Errorf("parse subnet: %w", err)
	}

	// Get all currently allocated IPs
	allocations, err := m.ListAllocations(ctx)
	if err != nil {
		return "", fmt.Errorf("list allocations: %w", err)
	}

	// Build set of used IPs
	usedIPs := make(map[string]bool)
	for _, alloc := range allocations {
		usedIPs[alloc.IP] = true
	}

	// Reserve network address and gateway
	usedIPs[ipNet.IP.String()] = true                 // Network address
	usedIPs[incrementIP(ipNet.IP, 1).String()] = true // Gateway (network + 1)

	// Calculate broadcast address
	broadcast := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		broadcast[i] = ipNet.IP[i] | ^ipNet.Mask[i]
	}
	usedIPs[broadcast.String()] = true // Broadcast address

	// Calculate subnet size (number of possible IPs)
	ones, bits := ipNet.Mask.Size()
	subnetSize := 1 << (bits - ones) // 2^(32-prefix_length)

	// Try up to 5 times to find a random available IP
	maxRetries := 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		// Generate random offset from network address (skip network and gateway)
		// Start from offset 2 to avoid network address (0) and gateway (1)
		randomOffset := mathrand.Intn(subnetSize-3) + 2

		// Calculate the random IP
		randomIP := incrementIP(ipNet.IP, randomOffset)

		// Check if IP is valid and available
		if ipNet.Contains(randomIP) {
			ipStr := randomIP.String()
			if !usedIPs[ipStr] {
				return ipStr, nil
			}
		}
	}

	// If random allocation failed after 5 attempts, fall back to sequential search
	// This handles the case where the subnet is nearly full
	for testIP := incrementIP(ipNet.IP, 2); ipNet.Contains(testIP); testIP = incrementIP(testIP, 1) {
		ipStr := testIP.String()
		if !usedIPs[ipStr] {
			return ipStr, nil
		}
	}

	return "", fmt.Errorf("no available IPs in subnet %s after %d random attempts and full scan", subnet, maxRetries)
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

// TAPPrefix is the prefix used for hypeman TAP devices
const TAPPrefix = "hype-"

// generateTAPName generates TAP device name from instance ID
func generateTAPName(instanceID string) string {
	// Use first 8 chars of instance ID
	// hype-{8chars} fits within 15-char Linux interface name limit
	shortID := instanceID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	return TAPPrefix + strings.ToLower(shortID)
}
