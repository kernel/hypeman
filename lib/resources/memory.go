package resources

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// MemoryResource implements Resource for memory discovery and tracking.
type MemoryResource struct {
	capacity       int64 // bytes
	instanceLister InstanceLister
}

// NewMemoryResource discovers host memory capacity from /proc/meminfo.
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

// detectMemoryCapacity reads /proc/meminfo to determine total memory.
func detectMemoryCapacity() (int64, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			// Format: "MemTotal:       16384000 kB"
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseInt(fields[1], 10, 64)
				if err != nil {
					return 0, fmt.Errorf("parse MemTotal: %w", err)
				}
				return kb * 1024, nil // Convert KB to bytes
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return 0, err
	}

	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}
