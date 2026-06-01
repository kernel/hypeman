//go:build darwin

package network

import (
	"context"
	"time"

	"github.com/kernel/hypeman/lib/logger"
)

// checkSubnetConflicts is a no-op on macOS as we use NAT networking.
func (m *manager) checkSubnetConflicts(ctx context.Context, subnet string) error {
	// NAT networking doesn't conflict with host routes
	return nil
}

// createBridge is a no-op on macOS as we use NAT networking.
// Virtualization.framework provides built-in NAT with NATNetworkDeviceAttachment.
func (m *manager) createBridge(ctx context.Context, name, gateway, subnet string) error {
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "macOS: skipping bridge creation (using NAT networking)")
	return nil
}

// setupIPTablesRules is a no-op on macOS as we use NAT networking.
func (m *manager) setupIPTablesRules(ctx context.Context, subnet, bridgeName string) error {
	return nil
}

// setupBridgeHTB is a no-op on macOS as we use NAT networking.
// macOS doesn't use traffic control qdiscs.
func (m *manager) setupBridgeHTB(ctx context.Context, bridgeName string, capacityBps int64) error {
	return nil
}

// createTAPDevice is a no-op on macOS as we use NAT networking.
// Virtualization.framework creates virtual network interfaces internally.
func (m *manager) createTAPDevice(ctx context.Context, tapName, bridgeName string, isolated bool) error {
	// On macOS with vz, network devices are created by the VMM itself
	return nil
}

func (m *manager) applyDownloadRateLimit(ctx context.Context, tapName string, rateLimitBps int64) error {
	return nil
}

func (m *manager) addVMClass(ctx context.Context, bridgeName, tapName string, rateBps, ceilBps int64) (string, error) {
	return "", nil
}

// deleteTAPDevice is a no-op on macOS as we use NAT networking.
func (m *manager) deleteTAPDevice(tapName, classID string) error {
	return nil
}

func (m *manager) tapDeviceExists(tapName string) bool {
	return true
}

// queryNetworkState returns a stub network state for macOS.
// On macOS, we use NAT which doesn't have a physical bridge.
func (m *manager) queryNetworkState(bridgeName string) (*Network, error) {
	// Return a virtual network representing macOS NAT
	// The actual IP will be assigned by Virtualization.framework's DHCP
	return &Network{
		Bridge:  "nat",
		Gateway: "192.168.64.1", // Default macOS vz NAT gateway
		Subnet:  "192.168.64.0/24",
	}, nil
}

// CleanupOrphanedTAPs is a no-op on macOS as we don't create TAP devices.
func (m *manager) CleanupOrphanedTAPs(ctx context.Context, preserveInstanceIDs []string, minAge time.Duration) int {
	return 0
}

// CleanupOrphanedClasses is a no-op on macOS as we don't use traffic control.
func (m *manager) CleanupOrphanedClasses(ctx context.Context) int {
	return 0
}
