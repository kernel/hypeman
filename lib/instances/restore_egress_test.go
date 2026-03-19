package instances

import (
	"testing"

	"github.com/kernel/hypeman/lib/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNetworkConfigFromAllocation_PreservesDNS(t *testing.T) {
	t.Parallel()

	alloc := &network.Allocation{
		IP:        "192.168.1.10",
		MAC:       "02:00:00:00:00:01",
		Gateway:   "192.168.1.1",
		Netmask:   "255.255.255.0",
		DNS:       "1.1.1.1",
		TAPDevice: "hype-abcd1234",
	}

	cfg := networkConfigFromAllocation(alloc)
	require.NotNil(t, cfg)
	assert.Equal(t, alloc.IP, cfg.IP)
	assert.Equal(t, alloc.MAC, cfg.MAC)
	assert.Equal(t, alloc.Gateway, cfg.Gateway)
	assert.Equal(t, alloc.Netmask, cfg.Netmask)
	assert.Equal(t, alloc.DNS, cfg.DNS)
	assert.Equal(t, alloc.TAPDevice, cfg.TAPDevice)
}

func TestRequiresRestoreConfigDiskRefresh(t *testing.T) {
	t.Parallel()

	assert.False(t, requiresRestoreConfigDiskRefresh(nil))
	assert.False(t, requiresRestoreConfigDiskRefresh(&StoredMetadata{
		NetworkEnabled: false,
		NetworkEgress:  &NetworkEgressPolicy{Enabled: true},
	}))
	assert.False(t, requiresRestoreConfigDiskRefresh(&StoredMetadata{
		NetworkEnabled: true,
	}))
	assert.False(t, requiresRestoreConfigDiskRefresh(&StoredMetadata{
		NetworkEnabled: true,
		NetworkEgress:  &NetworkEgressPolicy{Enabled: false},
	}))
	assert.True(t, requiresRestoreConfigDiskRefresh(&StoredMetadata{
		NetworkEnabled: true,
		NetworkEgress:  &NetworkEgressPolicy{Enabled: true},
	}))
}
