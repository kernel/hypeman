//go:build linux

package guestmemory

import (
	"context"
	"fmt"
	"os"
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
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, fmt.Errorf("read /proc/meminfo: %w", err)
	}

	total, available, err := parseLinuxMeminfo(string(data))
	if err != nil {
		return 0, 0, err
	}
	return total, available, nil
}

func readLinuxPSI() (bool, error) {
	data, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return false, fmt.Errorf("read /proc/pressure/memory: %w", err)
	}

	stressed, err := parseLinuxPSI(string(data))
	if err != nil {
		return false, err
	}
	return stressed, nil
}
