package guestmemory

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

const linuxPSIStressAvg10Threshold = 0.1

func parseLinuxMeminfo(data string) (int64, int64, error) {
	var total, available int64
	var sawTotal, sawAvailable bool

	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		switch fields[0] {
		case "MemTotal:":
			value, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("parse MemTotal: %w", err)
			}
			total = value * 1024
			sawTotal = true
		case "MemAvailable:":
			value, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("parse MemAvailable: %w", err)
			}
			available = value * 1024
			sawAvailable = true
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, fmt.Errorf("scan meminfo: %w", err)
	}
	if !sawTotal || !sawAvailable || total <= 0 || available < 0 {
		return 0, 0, fmt.Errorf("missing memory totals from /proc/meminfo")
	}

	return total, available, nil
}

func parseLinuxPSI(data string) (bool, error) {
	for _, line := range strings.Split(data, "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}

		fields := strings.Fields(line)
		for _, field := range fields[1:] {
			if !strings.HasPrefix(field, "avg10=") {
				continue
			}

			value, err := strconv.ParseFloat(strings.TrimPrefix(field, "avg10="), 64)
			if err != nil {
				return false, fmt.Errorf("parse psi avg10: %w", err)
			}
			return value >= linuxPSIStressAvg10Threshold, nil
		}
	}

	return false, nil
}

func parseDarwinVMStatOutput(vmStatOut, memsizeOut string) (int64, int64, error) {
	lines := strings.Split(vmStatOut, "\n")
	pageSize := int64(4096)
	var freePages, speculativePages int64

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "page size of") {
			parts := strings.Fields(line)
			for i := 0; i < len(parts); i++ {
				if parts[i] == "of" && i+1 < len(parts) {
					n, err := strconv.ParseInt(strings.TrimSuffix(parts[i+1], " bytes)"), 10, 64)
					if err == nil && n > 0 {
						pageSize = n
					}
					break
				}
			}
		}
		if strings.HasPrefix(line, "Pages free:") {
			n, err := parseDarwinPageCount(line)
			if err != nil {
				return 0, 0, err
			}
			freePages = n
		}
		if strings.HasPrefix(line, "Pages speculative:") {
			n, err := parseDarwinPageCount(line)
			if err != nil {
				return 0, 0, err
			}
			speculativePages = n
		}
	}

	total, err := strconv.ParseInt(strings.TrimSpace(memsizeOut), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse hw.memsize: %w", err)
	}

	available := (freePages + speculativePages) * pageSize
	return total, available, nil
}

func parseDarwinPageCount(line string) (int64, error) {
	parts := strings.Split(line, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("parse vm_stat line %q", line)
	}

	value := strings.TrimSpace(strings.TrimSuffix(parts[1], "."))
	return strconv.ParseInt(value, 10, 64)
}

func parseDarwinMemoryPressureOutput(out string) (bool, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "System-wide memory free percentage:") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) == 0 {
			break
		}

		last := strings.TrimSuffix(fields[len(fields)-1], "%")
		value, err := strconv.ParseInt(last, 10, 64)
		if err != nil {
			return false, fmt.Errorf("parse memory_pressure free percentage: %w", err)
		}
		return value <= 10, nil
	}

	return false, nil
}
