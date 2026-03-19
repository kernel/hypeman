//go:build linux

package guestmemory

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type linuxPressureSampler struct{}

func newHostPressureSampler() PressureSampler {
	return &linuxPressureSampler{}
}

func (s *linuxPressureSampler) Sample(ctx context.Context) (HostPressureSample, error) {
	_ = ctx

	total, available, err := readLinuxMeminfo()
	if err != nil {
		return HostPressureSample{}, err
	}
	stressed, err := readLinuxPSI()
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

func readLinuxMeminfo() (int64, int64, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, fmt.Errorf("open /proc/meminfo: %w", err)
	}
	defer file.Close()

	var total, available int64
	scanner := bufio.NewScanner(file)
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
		case "MemAvailable:":
			value, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("parse MemAvailable: %w", err)
			}
			available = value * 1024
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, fmt.Errorf("scan /proc/meminfo: %w", err)
	}
	if total <= 0 || available < 0 {
		return 0, 0, fmt.Errorf("missing memory totals from /proc/meminfo")
	}
	return total, available, nil
}

func readLinuxPSI() (bool, error) {
	data, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return false, fmt.Errorf("read /proc/pressure/memory: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "some ") {
			fields := strings.Fields(line)
			for _, field := range fields[1:] {
				if strings.HasPrefix(field, "avg10=") {
					value, err := strconv.ParseFloat(strings.TrimPrefix(field, "avg10="), 64)
					if err != nil {
						return false, fmt.Errorf("parse psi avg10: %w", err)
					}
					return value > 0, nil
				}
			}
		}
	}
	return false, nil
}
