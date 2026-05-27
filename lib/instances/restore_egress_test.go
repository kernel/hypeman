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

func TestGuestNetworkReconfigureCommand_AppliesAllocatedMAC(t *testing.T) {
	t.Parallel()

	alloc := &network.Allocation{
		IP:      "10.102.146.62",
		MAC:     "02:00:00:85:17:c8",
		Gateway: "10.102.0.1",
		Netmask: "255.255.0.0",
	}

	cmd, err := guestNetworkReconfigureCommand(alloc)
	require.NoError(t, err)
	assert.Contains(t, cmd, "ip link set dev eth0 down")
	assert.Contains(t, cmd, "ip link set dev eth0 address 02:00:00:85:17:c8")
	assert.Contains(t, cmd, "ip addr add 10.102.146.62/16 dev eth0")
	assert.Contains(t, cmd, "ip route replace default via 10.102.0.1 dev eth0")
	assert.Contains(t, cmd, "test \"$(cat /sys/class/net/eth0/address)\" = \"02:00:00:85:17:c8\"")
}

func TestGuestNetworkReconfigureCommand_RequiresAllocatedMAC(t *testing.T) {
	t.Parallel()

	_, err := guestNetworkReconfigureCommand(&network.Allocation{
		IP:      "10.102.146.62",
		Gateway: "10.102.0.1",
		Netmask: "255.255.0.0",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing network allocation MAC")
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
