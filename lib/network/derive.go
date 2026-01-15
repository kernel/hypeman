package network

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
)

// instanceMetadata is the minimal metadata we need to derive allocations
// Field names match StoredMetadata in lib/instances/types.go
type instanceMetadata struct {
	Name           string
	NetworkEnabled bool
	HypervisorType string
	IP             string // Assigned IP address
	MAC            string // Assigned MAC address
}

// deriveAllocation derives network allocation from CH or snapshot
func (m *manager) deriveAllocation(ctx context.Context, instanceID string) (*Allocation, error) {
	log := logger.FromContext(ctx)

	// 1. Load instance metadata to get instance name and network status
	meta, err := m.loadInstanceMetadata(instanceID)
	if err != nil {
		log.DebugContext(ctx, "failed to load instance metadata", "instance_id", instanceID, "error", err)
		return nil, err
	}

	// 2. If network not enabled, return nil
	if !meta.NetworkEnabled {
		return nil, nil
	}

	// 3. Get default network configuration for Gateway and Netmask
	defaultNet, err := m.getDefaultNetwork(ctx)
	if err != nil {
		return nil, fmt.Errorf("get default network: %w", err)
	}

	// Calculate netmask from subnet
	_, ipNet, err := net.ParseCIDR(defaultNet.Subnet)
	if err != nil {
		return nil, fmt.Errorf("parse subnet CIDR: %w", err)
	}
	netmask := fmt.Sprintf("%d.%d.%d.%d", ipNet.Mask[0], ipNet.Mask[1], ipNet.Mask[2], ipNet.Mask[3])

	// 4. Use stored metadata to derive allocation (works for all hypervisors)
	if meta.IP != "" && meta.MAC != "" {
		tap := generateTAPName(instanceID)

		// Determine state based on socket existence and snapshot
		socketPath := m.paths.InstanceSocket(instanceID, hypervisor.SocketNameForType(hypervisor.Type(meta.HypervisorType)))
		state := "stopped"
		if fileExists(socketPath) {
			state = "running"
		} else {
			// Check for snapshot (standby state)
			snapshotConfigJson := m.paths.InstanceSnapshotConfig(instanceID)
			if fileExists(snapshotConfigJson) {
				state = "standby"
			}
		}

		log.DebugContext(ctx, "derived allocation from metadata", "instance_id", instanceID, "state", state)
		return &Allocation{
			InstanceID:   instanceID,
			InstanceName: meta.Name,
			Network:      "default",
			IP:           meta.IP,
			MAC:          meta.MAC,
			TAPDevice:    tap,
			Gateway:      defaultNet.Gateway,
			Netmask:      netmask,
			State:        state,
		}, nil
	}

	// 5. No allocation (network not yet configured)
	return nil, nil
}

// GetAllocation gets the allocation for a specific instance
func (m *manager) GetAllocation(ctx context.Context, instanceID string) (*Allocation, error) {
	return m.deriveAllocation(ctx, instanceID)
}

// ListAllocations scans all guest directories and derives allocations
func (m *manager) ListAllocations(ctx context.Context) ([]Allocation, error) {
	guests, err := os.ReadDir(m.paths.GuestsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []Allocation{}, nil
		}
		return nil, fmt.Errorf("read guests dir: %w", err)
	}

	var allocations []Allocation
	for _, guest := range guests {
		if !guest.IsDir() {
			continue
		}
		alloc, err := m.deriveAllocation(ctx, guest.Name())
		if err == nil && alloc != nil {
			allocations = append(allocations, *alloc)
		}
	}
	return allocations, nil
}

// NameExists checks if instance name is already used in the default network
func (m *manager) NameExists(ctx context.Context, name string) (bool, error) {
	allocations, err := m.ListAllocations(ctx)
	if err != nil {
		return false, err
	}

	for _, alloc := range allocations {
		if alloc.InstanceName == name {
			return true, nil
		}
	}
	return false, nil
}

// loadInstanceMetadata loads minimal instance metadata
func (m *manager) loadInstanceMetadata(instanceID string) (*instanceMetadata, error) {
	metaPath := m.paths.InstanceMetadata(instanceID)

	data, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, fmt.Errorf("read metadata: %w", err)
	}

	var meta instanceMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}

	return &meta, nil
}

// fileExists checks if a file exists
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
