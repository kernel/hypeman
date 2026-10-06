//go:build linux

package resources

import (
	"context"
	"testing"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNetworkAdmissionCapacity(t *testing.T) {
	for _, tt := range []struct {
		configured string
		source     SourceType
		wantErr    bool
	}{
		{"", SourceUnknown, false},
		{"1Gbps", SourceConfigured, true},
		{"0Gbps", SourceConfigured, true},
	} {
		t.Run(string(tt.source)+tt.configured, func(t *testing.T) {
			cfg := &config.Config{Capacity: config.CapacityConfig{Network: tt.configured},
				Network:          config.NetworkConfig{UplinkInterface: "missing-test-interface"},
				Oversubscription: config.OversubscriptionConfig{Network: 1}}
			lister := &mockInstanceLister{allocations: []InstanceAllocation{
				{State: "Running", NetworkDownloadBps: 50_000_000},
			}}
			network, err := NewNetworkResource(context.Background(), cfg, lister)
			require.NoError(t, err)
			mgr := NewManager(cfg, paths.New(t.TempDir()))
			mgr.SetInstanceLister(lister)
			mgr.resources[ResourceNetwork] = network
			err = mgr.ReserveAllocation(context.Background(), "test", 0, 0, 125_000_001, 0, 0, 0, false)
			if tt.wantErr {
				require.ErrorContains(t, err, "insufficient network bandwidth")
			} else {
				require.NoError(t, err)
			}
			status, err := mgr.GetStatus(context.Background(), ResourceNetwork)
			require.NoError(t, err)
			assert.Equal(t, tt.source, status.Source)
			assert.Equal(t, int64(50_000_000), status.Allocated)
		})
	}
}
