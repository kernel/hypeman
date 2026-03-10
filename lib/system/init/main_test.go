package main

import (
	"testing"

	"github.com/kernel/hypeman/lib/vmconfig"
	"github.com/stretchr/testify/assert"
)

func TestShouldRunNetworkAndVolumesInParallel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *vmconfig.Config
		want bool
	}{
		{
			name: "false when network disabled",
			cfg: &vmconfig.Config{
				NetworkEnabled: false,
				VolumeMounts: []vmconfig.VolumeMount{
					{Path: "/mnt/data"},
				},
			},
			want: false,
		},
		{
			name: "false when no volumes",
			cfg: &vmconfig.Config{
				NetworkEnabled: true,
			},
			want: false,
		},
		{
			name: "true for disjoint mount paths",
			cfg: &vmconfig.Config{
				NetworkEnabled: true,
				VolumeMounts: []vmconfig.VolumeMount{
					{Path: "/mnt/data"},
					{Path: "var/lib/app-cache"},
				},
			},
			want: true,
		},
		{
			name: "false for root mount",
			cfg: &vmconfig.Config{
				NetworkEnabled: true,
				VolumeMounts: []vmconfig.VolumeMount{
					{Path: "/"},
				},
			},
			want: false,
		},
		{
			name: "false for etc mount",
			cfg: &vmconfig.Config{
				NetworkEnabled: true,
				VolumeMounts: []vmconfig.VolumeMount{
					{Path: "/etc"},
				},
			},
			want: false,
		},
		{
			name: "false for etc subtree mount",
			cfg: &vmconfig.Config{
				NetworkEnabled: true,
				VolumeMounts: []vmconfig.VolumeMount{
					{Path: "/etc/resolv.conf"},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, shouldRunNetworkAndVolumesInParallel(tt.cfg))
		})
	}
}
