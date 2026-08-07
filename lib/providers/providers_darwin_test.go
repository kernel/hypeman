//go:build darwin

package providers

import (
	"testing"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuilderRegistryURLUsesDarwinEffectiveGateway(t *testing.T) {
	cfg := &config.Config{
		Network: config.NetworkConfig{
			BridgeName:    "vmbr0",
			SubnetCIDR:    "10.100.0.0/16",
			SubnetGateway: "10.100.0.1",
		},
	}
	networkManager := network.NewManager(paths.New(t.TempDir()), cfg, nil)

	for _, registryURL := range []string{"", "localhost:4973", "127.0.0.1:5000"} {
		resolved, err := builderRegistryURL(registryURL, networkManager)
		require.NoError(t, err)
		if registryURL == "127.0.0.1:5000" {
			assert.Equal(t, "192.168.64.1:5000", resolved)
		} else {
			assert.Equal(t, "192.168.64.1:4973", resolved)
		}
	}
}
