package instances

import (
	"bufio"
	"os"
	"strconv"
	"strings"

	"github.com/kernel/hypeman/lib/vmm"
)

// HostTopology represents the CPU topology of the host machine
type HostTopology struct {
	ThreadsPerCore int
	CoresPerSocket int
	Sockets        int
}

// detectHostTopology reads /proc/cpuinfo to determine the host's CPU topology
func detectHostTopology() *HostTopology {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return nil
	}
	defer file.Close()

	var (
		siblings      int
		cpuCores      int
		physicalIDs   = make(map[int]bool)
		hasSiblings   bool
		hasCpuCores   bool
		hasPhysicalID bool
	)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		
		// Parse key: value pairs
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
		case "cpu cores":
			if !hasCpuCores {
				cpuCores, _ = strconv.Atoi(value)
				hasCpuCores = true
			}
		case "physical id":
			physicalID, _ := strconv.Atoi(value)
			physicalIDs[physicalID] = true
			hasPhysicalID = true
		}
	}

	if err := scanner.Err(); err != nil {
		return nil
	}

	// Validate we have the necessary information
	if !hasSiblings || !hasCpuCores || !hasPhysicalID || cpuCores == 0 {
		return nil
	}

	threadsPerCore := siblings / cpuCores
	if threadsPerCore < 1 {
		threadsPerCore = 1
	}

	sockets := len(physicalIDs)
	if sockets < 1 {
		sockets = 1
	}

	return &HostTopology{
		ThreadsPerCore: threadsPerCore,
		CoresPerSocket: cpuCores,
		Sockets:        sockets,
	}
}

// calculateGuestTopology determines an optimal guest CPU topology based on
// the requested vCPU count and the host's topology
func calculateGuestTopology(vcpus int, host *HostTopology) *vmm.CpuTopology {
	// For very small VMs, let Cloud Hypervisor use its defaults
	if vcpus <= 2 {
		return nil
	}

	// If we couldn't detect host topology, don't specify guest topology
	if host == nil {
		return nil
	}

	var threadsPerCore, coresPerDie, diesPerPackage, packages int

	// Try to match host's threads per core if vCPUs are divisible by it
	if host.ThreadsPerCore > 1 && vcpus%host.ThreadsPerCore == 0 {
		threadsPerCore = host.ThreadsPerCore
		remainingCores := vcpus / threadsPerCore
		
		// Distribute cores across sockets if needed
		if remainingCores <= host.CoresPerSocket {
			coresPerDie = remainingCores
			diesPerPackage = 1
			packages = 1
		} else if remainingCores%(host.CoresPerSocket) == 0 {
			coresPerDie = host.CoresPerSocket
			diesPerPackage = 1
			packages = remainingCores / host.CoresPerSocket
		} else {
			// Can't cleanly distribute, try simpler topology
			coresPerDie = remainingCores
			diesPerPackage = 1
			packages = 1
		}
	} else {
		// Use 1 thread per core for simpler layout
		threadsPerCore = 1
		
		if vcpus <= host.CoresPerSocket {
			coresPerDie = vcpus
			diesPerPackage = 1
			packages = 1
		} else if vcpus%(host.CoresPerSocket) == 0 {
			coresPerDie = host.CoresPerSocket
			diesPerPackage = 1
			packages = vcpus / host.CoresPerSocket
		} else {
			// Can't cleanly distribute, use simple topology
			coresPerDie = vcpus
			diesPerPackage = 1
			packages = 1
		}
	}

	// Validate the topology multiplies to vcpus
	if threadsPerCore*coresPerDie*diesPerPackage*packages != vcpus {
		return nil
	}

	// Validate all values are within Cloud Hypervisor's u8 limits
	if threadsPerCore > 255 || coresPerDie > 255 || diesPerPackage > 255 || packages > 255 {
		return nil
	}

	return &vmm.CpuTopology{
		ThreadsPerCore: &threadsPerCore,
		CoresPerDie:    &coresPerDie,
		DiesPerPackage: &diesPerPackage,
		Packages:       &packages,
	}
}

