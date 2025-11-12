package network

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/vmm"
)

// instanceMetadata is the minimal metadata we need to derive allocations
type instanceMetadata struct {
	Name    string `json:"name"`
	Network string `json:"network"`
}

// deriveAllocation derives network allocation from CH or snapshot
func (m *manager) deriveAllocation(ctx context.Context, instanceID string) (*Allocation, error) {
	log := logger.FromContext(ctx)

	// 1. Load instance metadata to get network name and instance name
	meta, err := m.loadInstanceMetadata(instanceID)
	if err != nil {
		log.DebugContext(ctx, "failed to load instance metadata", "instance_id", instanceID, "error", err)
		return nil, err
	}

	// 2. If no network configured, return nil
	if meta.Network == "" {
		return nil, nil
	}

	// 3. Try to derive from running VM first
	socketPath := m.paths.InstanceSocket(instanceID)
	if fileExists(socketPath) {
		client, err := vmm.NewVMM(socketPath)
		if err == nil {
			resp, err := client.GetVmInfoWithResponse(ctx)
			if err == nil && resp.JSON200 != nil && resp.JSON200.Config.Net != nil && len(*resp.JSON200.Config.Net) > 0 {
				nets := *resp.JSON200.Config.Net
				net := nets[0]
				if net.Ip != nil && net.Mac != nil && net.Tap != nil {
					log.DebugContext(ctx, "derived allocation from running VM", "instance_id", instanceID)
					return &Allocation{
						InstanceID:   instanceID,
						InstanceName: meta.Name,
						Network:      meta.Network,
						IP:           *net.Ip,
						MAC:          *net.Mac,
						TAPDevice:    *net.Tap,
						State:        "running",
					}, nil
				}
			}
		}
	}

	// 4. Try to derive from snapshot
	snapshotVmJson := filepath.Join(m.paths.InstanceSnapshotLatest(instanceID), "vm.json")
	if fileExists(snapshotVmJson) {
		vmConfig, err := m.parseVmJson(snapshotVmJson)
		if err == nil && vmConfig.Net != nil && len(*vmConfig.Net) > 0 {
			nets := *vmConfig.Net
			if nets[0].Ip != nil && nets[0].Mac != nil && nets[0].Tap != nil {
				log.DebugContext(ctx, "derived allocation from snapshot", "instance_id", instanceID)
				return &Allocation{
					InstanceID:   instanceID,
					InstanceName: meta.Name,
					Network:      meta.Network,
					IP:           *nets[0].Ip,
					MAC:          *nets[0].Mac,
					TAPDevice:    *nets[0].Tap,
					State:        "standby",
				}, nil
			}
		}
	}

	// 5. No allocation (stopped or network not yet configured)
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

// NameExistsInNetwork checks if instance name is already used in network
func (m *manager) NameExistsInNetwork(ctx context.Context, name, network string) (bool, error) {
	allocations, err := m.ListAllocations(ctx)
	if err != nil {
		return false, err
	}

	for _, alloc := range allocations {
		if alloc.InstanceName == name && alloc.Network == network {
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

// parseVmJson parses Cloud Hypervisor's vm.json from snapshot
func (m *manager) parseVmJson(path string) (*vmm.VmConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read vm.json: %w", err)
	}

	var vmConfig vmm.VmConfig
	if err := json.Unmarshal(data, &vmConfig); err != nil {
		return nil, fmt.Errorf("unmarshal vm.json: %w", err)
	}

	return &vmConfig, nil
}

// fileExists checks if a file exists
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

