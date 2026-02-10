//go:build linux

package resources

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/vishvananda/netlink"
)

// NetworkResource implements Resource for network bandwidth discovery and tracking.
type NetworkResource struct {
	capacity       int64 // bytes per second
	instanceLister InstanceLister
}

// NewNetworkResource discovers network capacity.
// If cfg.NetworkLimit is set, uses that; otherwise auto-detects from uplink interface.
func NewNetworkResource(ctx context.Context, cfg *config.Config, instLister InstanceLister) (*NetworkResource, error) {
	var capacity int64
	log := logger.FromContext(ctx)

	if cfg.NetworkLimit != "" {
		// Parse configured limit (e.g., "10Gbps", "1GB/s")
		parsed, err := ParseBandwidth(cfg.NetworkLimit)
		if err != nil {
			return nil, fmt.Errorf("parse network limit: %w", err)
		}
		capacity = parsed
	} else {
		// Auto-detect from uplink interface
		uplink, err := getUplinkInterface(cfg.UplinkInterface)
		if err != nil {
			// No uplink found - network limiting disabled
			log.WarnContext(ctx, "no uplink interface found, network limiting disabled", "error", err)
			capacity = 0
		} else {
			speed, err := getInterfaceSpeed(uplink)
			if err != nil || speed <= 0 {
				// Speed detection failed - network limiting disabled
				log.WarnContext(ctx, "failed to detect interface speed, network limiting disabled", "interface", uplink, "error", err, "speed", speed)
				capacity = 0
			} else {
				// speed is in Mbps, convert to bytes/sec
				capacity = speed * 1000 * 1000 / 8
			}
		}
	}

	return &NetworkResource{
		capacity:       capacity,
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

// Allocated returns total network bandwidth allocated to running instances.
// Uses the max of download/upload per instance since they share the physical link.
func (n *NetworkResource) Allocated(ctx context.Context) (int64, error) {
	if n.instanceLister == nil {
		return 0, nil
	}

	instances, err := n.instanceLister.ListInstanceAllocations(ctx)
	if err != nil {
		return 0, err
	}

	var total int64
	for _, inst := range instances {
		if isActiveState(inst.State) {
			// Use max of download/upload since they share the same physical link
			// This is conservative - actual usage depends on traffic direction
			alloc := inst.NetworkDownloadBps
			if inst.NetworkUploadBps > alloc {
				alloc = inst.NetworkUploadBps
			}
			total += alloc
		}
	}
	return total, nil
}

// getUplinkInterface returns the uplink interface name.
// Uses explicit config if set, otherwise auto-detects from default route.
func getUplinkInterface(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}

	// Auto-detect from default route
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return "", fmt.Errorf("list routes: %w", err)
	}

	for _, route := range routes {
		// Default route has no destination (Dst is nil or 0.0.0.0/0)
		if route.Dst == nil || route.Dst.IP.IsUnspecified() {
			if route.LinkIndex > 0 {
				link, err := netlink.LinkByIndex(route.LinkIndex)
				if err == nil {
					return link.Attrs().Name, nil
				}
			}
		}
	}

	return "", fmt.Errorf("no default route found")
}

// getInterfaceSpeed reads the link speed from /sys/class/net/{iface}/speed.
// Returns speed in Mbps, or -1 for virtual interfaces.
func getInterfaceSpeed(iface string) (int64, error) {
	path := fmt.Sprintf("/sys/class/net/%s/speed", iface)
	data, err := os.ReadFile(path)
	if err != nil {
		return -1, err
	}

	speed, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return -1, err
	}

	return speed, nil
}
