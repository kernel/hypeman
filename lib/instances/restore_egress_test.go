package instances

import (
	"encoding/binary"
	"encoding/json"
	"os"
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

func TestGuestNetworkReconfigureConfig_AppliesAllocatedMAC(t *testing.T) {
	t.Parallel()

	alloc := &network.Allocation{
		IP:      "10.102.146.62",
		MAC:     "02:00:00:85:17:c8",
		Gateway: "10.102.0.1",
		Netmask: "255.255.0.0",
	}

	cfg, err := guestNetworkReconfigureConfig(alloc)
	require.NoError(t, err)
	assert.Equal(t, "10.102.146.62", cfg.ip)
	assert.Equal(t, "02:00:00:85:17:c8", cfg.mac)
	assert.Equal(t, "10.102.0.1", cfg.gateway)
	assert.Equal(t, 16, cfg.prefix)
}

func TestGuestNetworkReconfigureCommand_FallbackPreservesShellBehavior(t *testing.T) {
	t.Parallel()

	alloc := &network.Allocation{
		IP:      "10.102.146.62",
		MAC:     "02:00:00:85:17:c8",
		Gateway: "10.102.0.1",
		Netmask: "255.255.0.0",
	}

	cmd, err := guestNetworkReconfigureCommand(alloc)
	require.NoError(t, err)
	assert.Contains(t, cmd, "ip link set dev eth0 address 02:00:00:85:17:c8")
	assert.Contains(t, cmd, "ip addr add 10.102.146.62/16 dev eth0")
	assert.Contains(t, cmd, "(ip neigh flush dev eth0 || true)")
}

func TestGuestNetworkReconfigureConfig_RequiresAllocatedMAC(t *testing.T) {
	t.Parallel()

	_, err := guestNetworkReconfigureConfig(&network.Allocation{
		IP:      "10.102.146.62",
		Gateway: "10.102.0.1",
		Netmask: "255.255.0.0",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing network allocation MAC")
}

func TestPatchGuestResumeNetworkMailbox(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	token := "test-token"
	mem := make([]byte, 4096)
	copy(mem[512:], guestResumeNetworkMailboxMagic)
	copy(mem[512+len(guestResumeNetworkMailboxMagic):], token)
	require.NoError(t, os.WriteFile(dir+"/"+firecrackerSnapshotMemoryFile, mem, 0644))

	payload := &guestResumeNetworkPayload{
		InterfaceName: "eth0",
		MAC:           "02:00:00:85:17:c8",
		IPv4:          "10.102.146.62",
		Prefix:        16,
		Gateway:       "10.102.0.1",
		AckPort:       43210,
	}
	require.NoError(t, patchGuestResumeNetworkMailbox(dir, token, payload))

	patched, err := os.ReadFile(dir + "/" + firecrackerSnapshotMemoryFile)
	require.NoError(t, err)

	offset := 512
	require.Equal(t, uint32(1), binary.LittleEndian.Uint32(patched[offset+guestResumeNetworkMailboxSeqOffset:]))
	payloadLen := binary.LittleEndian.Uint32(patched[offset+guestResumeNetworkMailboxLengthOffset:])
	require.NotZero(t, payloadLen)

	var decoded guestResumeNetworkPayload
	err = json.Unmarshal(patched[offset+guestResumeNetworkMailboxPayloadOffset:offset+guestResumeNetworkMailboxPayloadOffset+int(payloadLen)], &decoded)
	require.NoError(t, err)
	assert.Equal(t, *payload, decoded)
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
