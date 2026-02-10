//go:build darwin

package resources

import (
	"context"

	"github.com/kernel/hypeman/cmd/api/config"
)

// NetworkResource implements Resource for network bandwidth discovery and tracking.
// On macOS, network rate limiting is not supported.
type NetworkResource struct {
	capacity       int64 // bytes per second (set to high value on macOS)
	instanceLister InstanceLister
}

// NewNetworkResource creates a network resource on macOS.
// Network capacity detection and rate limiting are not supported on macOS.
func NewNetworkResource(ctx context.Context, cfg *config.Config, instLister InstanceLister) (*NetworkResource, error) {
	// Default to 10 Gbps as a reasonable high limit on macOS
	// Network rate limiting is not enforced on macOS
	return &NetworkResource{
		capacity:       10 * 1024 * 1024 * 1024 / 8, // 10 Gbps in bytes/sec
		instanceLister: instLister,
	}, nil
}

// Type returns the resource type.
func (n *NetworkResource) Type() ResourceType {
	return ResourceNetwork
}

// Capacity returns the network capacity in bytes per second.
func (n *NetworkResource) Capacity() int64 {
	return n.capacity
}

// Allocated returns currently allocated network bandwidth.
// On macOS, this is always 0 as rate limiting is not supported.
func (n *NetworkResource) Allocated(ctx context.Context) (int64, error) {
	return 0, nil
}

// AvailableFor returns available network bandwidth.
// On macOS, this always returns the full capacity.
func (n *NetworkResource) AvailableFor(ctx context.Context, requested int64) (int64, error) {
	return n.capacity, nil
}
