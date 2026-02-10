package resources

import (
	"context"
)

// MemoryResource implements Resource for memory discovery and tracking.
type MemoryResource struct {
	capacity       int64 // bytes
	instanceLister InstanceLister
}

// NewMemoryResource discovers host memory capacity.
func NewMemoryResource() (*MemoryResource, error) {
	capacity, err := detectMemoryCapacity()
	if err != nil {
		return nil, err
	}
	return &MemoryResource{capacity: capacity}, nil
}

// SetInstanceLister sets the instance lister for allocation calculations.
func (m *MemoryResource) SetInstanceLister(lister InstanceLister) {
	m.instanceLister = lister
}

// Type returns the resource type.
func (m *MemoryResource) Type() ResourceType {
	return ResourceMemory
}

// Capacity returns the total memory in bytes available on the host.
func (m *MemoryResource) Capacity() int64 {
	return m.capacity
}

// Allocated returns the total memory allocated to running instances.
func (m *MemoryResource) Allocated(ctx context.Context) (int64, error) {
	if m.instanceLister == nil {
		return 0, nil
	}

	instances, err := m.instanceLister.ListInstanceAllocations(ctx)
	if err != nil {
		return 0, err
	}

	var total int64
	for _, inst := range instances {
		if isActiveState(inst.State) {
			total += inst.MemoryBytes
		}
	}
	return total, nil
}
