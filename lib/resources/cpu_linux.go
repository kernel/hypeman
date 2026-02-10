//go:build linux

package resources

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

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
