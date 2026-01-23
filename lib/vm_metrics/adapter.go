package vm_metrics

import (
	"context"

	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/resources"
)

// InstanceManagerAdapter adapts an instance manager that returns resources.InstanceUtilizationInfo
// to the vm_metrics.InstanceSource interface.
type InstanceManagerAdapter struct {
	manager interface {
		ListRunningInstancesInfo(ctx context.Context) ([]resources.InstanceUtilizationInfo, error)
	}
}

// NewInstanceManagerAdapter creates an adapter for the given instance manager.
func NewInstanceManagerAdapter(manager interface {
	ListRunningInstancesInfo(ctx context.Context) ([]resources.InstanceUtilizationInfo, error)
}) *InstanceManagerAdapter {
	return &InstanceManagerAdapter{manager: manager}
}

// ListRunningInstancesForMetrics implements InstanceSource.
func (a *InstanceManagerAdapter) ListRunningInstancesForMetrics() ([]InstanceInfo, error) {
	ctx := context.Background()
	infos, err := a.manager.ListRunningInstancesInfo(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]InstanceInfo, len(infos))
	for i, info := range infos {
		result[i] = InstanceInfo{
			ID:                   info.ID,
			Name:                 info.Name,
			HypervisorPID:        info.HypervisorPID,
			TAPDevice:            info.TAPDevice,
			AllocatedVcpus:       info.AllocatedVcpus,
			AllocatedMemoryBytes: info.AllocatedMemoryBytes,
		}
	}
	return result, nil
}

// InstanceListerAdapter adapts an instance lister that provides Instance structs
// to the vm_metrics.InstanceSource interface.
// This is useful when you need to build InstanceInfo directly from Instance data.
type InstanceListerAdapter struct {
	listFunc func(ctx context.Context) ([]InstanceData, error)
}

// InstanceData contains the minimal instance data needed for metrics.
type InstanceData struct {
	ID                   string
	Name                 string
	HypervisorPID        *int
	NetworkEnabled       bool
	AllocatedVcpus       int
	AllocatedMemoryBytes int64
}

// NewInstanceListerAdapter creates an adapter with a custom list function.
func NewInstanceListerAdapter(listFunc func(ctx context.Context) ([]InstanceData, error)) *InstanceListerAdapter {
	return &InstanceListerAdapter{listFunc: listFunc}
}

// ListRunningInstancesForMetrics implements InstanceSource.
func (a *InstanceListerAdapter) ListRunningInstancesForMetrics() ([]InstanceInfo, error) {
	ctx := context.Background()
	instances, err := a.listFunc(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]InstanceInfo, len(instances))
	for i, inst := range instances {
		info := InstanceInfo{
			ID:                   inst.ID,
			Name:                 inst.Name,
			HypervisorPID:        inst.HypervisorPID,
			AllocatedVcpus:       inst.AllocatedVcpus,
			AllocatedMemoryBytes: inst.AllocatedMemoryBytes,
		}
		if inst.NetworkEnabled {
			info.TAPDevice = network.GenerateTAPName(inst.ID)
		}
		result[i] = info
	}
	return result, nil
}
