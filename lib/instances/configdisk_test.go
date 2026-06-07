package instances

import (
	"context"
	"testing"

	"github.com/kernel/hypeman/lib/images"
	"github.com/stretchr/testify/assert"
)

func TestBuildGuestConfigEnableRosetta(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
	}{
		{name: "enabled", enabled: true},
		{name: "disabled", enabled: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &manager{}
			inst := &Instance{}
			inst.EnableRosetta = tt.enabled

			cfg := m.buildGuestConfig(context.Background(), inst, &images.Image{}, nil, nil)

			assert.Equal(t, tt.enabled, cfg.EnableRosetta)
		})
	}
}
