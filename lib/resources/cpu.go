package resources

import (
	"context"
)

// CPUResource implements Resource for CPU discovery and tracking.
type CPUResource struct {
	capacity       int64
	instanceLister InstanceLister
}

// NewCPUResource discovers host CPU capacity.
func NewCPUResource() (*CPUResource, error) {
	capacity, err := detectCPUCapacity()
	if err != nil {
		return nil, err
	}
	return &CPUResource{capacity: capacity}, nil
}

// SetInstanceLister sets the instance lister for allocation calculations.
func (c *CPUResource) SetInstanceLister(lister InstanceLister) {
	c.instanceLister = lister
}

// Type returns the resource type.
func (c *CPUResource) Type() ResourceType {
	return ResourceCPU
}

// Capacity returns the total number of vCPUs available on the host.
func (c *CPUResource) Capacity() int64 {
	return c.capacity
}

// Allocated returns the total vCPUs allocated to running instances.
func (c *CPUResource) Allocated(ctx context.Context) (int64, error) {
	if c.instanceLister == nil {
		return 0, nil
	}

	instances, err := c.instanceLister.ListInstanceAllocations(ctx)
	if err != nil {
		return 0, err
	}

	var total int64
	for _, inst := range instances {
		if isActiveState(inst.State) {
			total += int64(inst.Vcpus)
		}
	}
	return total, nil
}

// isActiveState returns true if the instance state indicates it's consuming resources.
func isActiveState(state string) bool {
	switch state {
	case "Running", "Paused", "Created":
		return true
	default:
		return false
	}
}
