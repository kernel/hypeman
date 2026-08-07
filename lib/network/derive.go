package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"go.opentelemetry.io/otel/attribute"
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

	// 3. Derive gateway/netmask from the same platform-effective network used
	// for new allocations. This remains available without querying transient live
	// bridge state and prevents raw config from overriding VZ's NAT network.
	defaultNetwork, err := m.EffectiveDefaultNetwork()
	if err != nil {
		return nil, fmt.Errorf("get effective default network: %w", err)
	}
	_, ipNet, err := net.ParseCIDR(defaultNetwork.Subnet)
	if err != nil {
		return nil, fmt.Errorf("parse subnet CIDR: %w", err)
	}
	netmask := fmt.Sprintf("%d.%d.%d.%d", ipNet.Mask[0], ipNet.Mask[1], ipNet.Mask[2], ipNet.Mask[3])

	// 4. Use stored metadata to derive allocation (works for all hypervisors)
	if meta.IP != "" && meta.MAC != "" {
		tap := GenerateTAPName(instanceID)

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
			Gateway:      defaultNetwork.Gateway,
			Netmask:      netmask,
			DNS:          m.config.Network.DNSServer,
			State:        state,
			ClassID:      m.loadClassID(instanceID),
		}, nil
	}

	// 5. No allocation (network not yet configured)
	return nil, nil
}

// GetAllocation gets the allocation for a specific instance
func (m *manager) GetAllocation(ctx context.Context, instanceID string) (*Allocation, error) {
	alloc, err := m.deriveAllocation(ctx, instanceID)
	if err == nil && alloc != nil {
		return alloc, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if pending, ok := m.pendingAllocations[instanceID]; ok {
		alloc := pending.allocation
		if alloc.ClassID == "" {
			alloc.ClassID = m.loadClassID(instanceID)
		}
		return &alloc, nil
	}
	return alloc, err
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

// NameExists checks if instance name is already used in the default network.
// excludeInstanceID allows excluding a specific instance from the check (used when
// starting an existing instance to avoid it conflicting with itself).
func (m *manager) NameExists(ctx context.Context, name string, excludeInstanceID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	checkCtx, checkSpanEnd := startNetworkStep(ctx, "network.name_exists",
		attribute.String("operation", "name_exists"),
	)
	var checkErr error
	defer func() {
		checkSpanEnd(checkErr)
	}()

	listCtx, listSpanEnd := startNetworkStep(checkCtx, "network.list_allocations",
		attribute.String("operation", "list_allocations"),
		attribute.String("caller", "name_exists"),
	)
	allocations, err := m.listAllocationsWithPendingLocked(listCtx)
	listSpanEnd(err)
	if err != nil {
		checkErr = err
		return false, err
	}

	return nameExistsInAllocations(allocations, name, excludeInstanceID), nil
}

func (m *manager) listAllocationsWithPendingLocked(ctx context.Context) ([]Allocation, error) {
	allocations, err := m.ListAllocations(ctx)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(allocations))
	for _, alloc := range allocations {
		seen[alloc.InstanceID] = struct{}{}
		delete(m.pendingAllocations, alloc.InstanceID)
	}
	for id, pending := range m.pendingAllocations {
		if _, ok := seen[id]; ok {
			continue
		}
		alloc := pending.allocation
		if alloc.ClassID == "" {
			alloc.ClassID = m.loadClassID(id)
		}
		allocations = append(allocations, alloc)
	}
	return allocations, nil
}

func (m *manager) rememberPendingAllocationLocked(alloc Allocation) {
	if m.pendingAllocations == nil {
		m.pendingAllocations = make(map[string]pendingAllocation)
	}
	m.pendingAllocations[alloc.InstanceID] = pendingAllocation{
		allocation: alloc,
	}
}

func (m *manager) forgetPendingAllocation(instanceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pendingAllocations, instanceID)
}

func nameExistsInAllocations(allocations []Allocation, name, excludeInstanceID string) bool {
	for _, alloc := range allocations {
		if excludeInstanceID != "" && alloc.InstanceID == excludeInstanceID {
			continue
		}
		if alloc.InstanceName == name {
			return true
		}
	}
	return false
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
