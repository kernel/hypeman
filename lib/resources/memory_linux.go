//go:build linux

package resources

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

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
