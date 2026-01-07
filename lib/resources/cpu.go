package resources

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// CPUResource implements Resource for CPU discovery and tracking.
type CPUResource struct {
	capacity       int64
	instanceLister InstanceLister
}

// NewCPUResource discovers host CPU capacity from /proc/cpuinfo.
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

// detectCPUCapacity reads /proc/cpuinfo to determine total vCPU count.
// Returns threads × cores × sockets.
func detectCPUCapacity() (int64, error) {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return 0, fmt.Errorf("open /proc/cpuinfo: %w", err)
	}
	defer file.Close()

	var (
		siblings      int
		physicalIDs   = make(map[int]bool)
		hasSiblings   bool
		hasPhysicalID bool
	)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		switch key {
		case "siblings":
			if !hasSiblings {
				siblings, _ = strconv.Atoi(value)
				hasSiblings = true
			}
		case "physical id":
			physicalID, _ := strconv.Atoi(value)
			physicalIDs[physicalID] = true
			hasPhysicalID = true
		}
	}

	if err := scanner.Err(); err != nil {
		return 0, err
	}

	// Calculate total vCPUs
	if hasSiblings && hasPhysicalID {
		// siblings = threads per socket, physicalIDs = number of sockets
		sockets := len(physicalIDs)
		if sockets < 1 {
			sockets = 1
		}
		return int64(siblings * sockets), nil
	}

	// Fallback: count processor entries
	file.Seek(0, 0)
	scanner = bufio.NewScanner(file)
	count := 0
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "processor") {
			count++
		}
	}
	if count > 0 {
		return int64(count), nil
	}

	// Ultimate fallback
	return 1, nil
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
