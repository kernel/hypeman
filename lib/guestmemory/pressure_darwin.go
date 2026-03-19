//go:build darwin

package guestmemory

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type darwinPressureSampler struct{}

func newHostPressureSampler() PressureSampler {
	return &darwinPressureSampler{}
}

func (s *darwinPressureSampler) Sample(ctx context.Context) (HostPressureSample, error) {
	total, available, err := readDarwinVMStat(ctx)
	if err != nil {
		return HostPressureSample{}, err
	}
	stressed, err := readDarwinMemoryPressure(ctx)
	if err != nil {
		return HostPressureSample{}, err
	}

	return HostPressureSample{
		TotalBytes:       total,
		AvailableBytes:   available,
		AvailablePercent: percentage(available, total),
		Stressed:         stressed,
	}, nil
}

func readDarwinVMStat(ctx context.Context) (int64, int64, error) {
	out, err := exec.CommandContext(ctx, "vm_stat").Output()
	if err != nil {
		return 0, 0, fmt.Errorf("run vm_stat: %w", err)
	}
	lines := strings.Split(string(out), "\n")
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

	memsizeOut, err := exec.CommandContext(ctx, "sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, 0, fmt.Errorf("run sysctl hw.memsize: %w", err)
	}
	total, err := strconv.ParseInt(strings.TrimSpace(string(memsizeOut)), 10, 64)
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

func readDarwinMemoryPressure(ctx context.Context) (bool, error) {
	out, err := exec.CommandContext(ctx, "memory_pressure", "-Q").Output()
	if err != nil {
		return false, fmt.Errorf("run memory_pressure -Q: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "System-wide memory free percentage:") {
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
	}
	return false, nil
}
