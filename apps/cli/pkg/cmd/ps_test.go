package cmd

import (
	"testing"

	"github.com/kernel/hypeman-go"
	"github.com/stretchr/testify/assert"
)

func TestFormatGPU(t *testing.T) {
	tests := []struct {
		name     string
		gpu      hypeman.InstanceGPU
		expected string
	}{
		{
			name:     "no GPU",
			gpu:      hypeman.InstanceGPU{},
			expected: "-",
		},
		{
			name: "vGPU with profile",
			gpu: hypeman.InstanceGPU{
				Profile:  "L40S-1Q",
				MdevUuid: "abc-123",
			},
			expected: "L40S-1Q",
		},
		{
			name: "vGPU without profile but with mdev",
			gpu: hypeman.InstanceGPU{
				MdevUuid: "abc-123",
			},
			expected: "vgpu",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatGPU(tt.gpu)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFormatHypervisor(t *testing.T) {
	tests := []struct {
		name       string
		hypervisor hypeman.InstanceHypervisor
		expected   string
	}{
		{
			name:       "cloud-hypervisor",
			hypervisor: hypeman.InstanceHypervisorCloudHypervisor,
			expected:   "ch",
		},
		{
			name:       "qemu",
			hypervisor: hypeman.InstanceHypervisorQemu,
			expected:   "qemu",
		},
		{
			name:       "empty defaults to ch",
			hypervisor: "",
			expected:   "ch",
		},
		{
			name:       "unknown value",
			hypervisor: hypeman.InstanceHypervisor("unknown"),
			expected:   "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatHypervisor(tt.hypervisor)
			assert.Equal(t, tt.expected, result)
		})
	}
}
