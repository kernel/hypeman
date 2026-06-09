//go:build darwin

package vz

import (
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
)

func TestBuildShimConfigFromVMConfigEnableRosetta(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
	}{
		{name: "enabled", enabled: true},
		{name: "disabled", enabled: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := buildShimConfigFromVMConfig(hypervisor.VMConfig{EnableRosetta: tt.enabled}, "/tmp/instance/vz.sock")
			assert.Equal(t, tt.enabled, cfg.EnableRosetta)
		})
	}
}
