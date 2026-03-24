//go:build darwin

package guestmemory

import (
	"context"
	"fmt"
	"os/exec"
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

	memsizeOut, err := exec.CommandContext(ctx, "sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, 0, fmt.Errorf("run sysctl hw.memsize: %w", err)
	}

	total, available, err := parseDarwinVMStatOutput(string(out), string(memsizeOut))
	if err != nil {
		return 0, 0, err
	}
	return total, available, nil
}

func readDarwinMemoryPressure(ctx context.Context) (bool, error) {
	out, err := exec.CommandContext(ctx, "memory_pressure", "-Q").Output()
	if err != nil {
		return false, fmt.Errorf("run memory_pressure -Q: %w", err)
	}

	stressed, err := parseDarwinMemoryPressureOutput(string(out))
	if err != nil {
		return false, err
	}
	return stressed, nil
}
