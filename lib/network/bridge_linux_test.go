//go:build linux

package network

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestParseBridgeFilters(t *testing.T) {
	output := `filter parent 1: protocol all pref 1 basic chain 0
filter parent 1: protocol all pref 1 basic chain 0 handle 0x1 flowid 1:a3f2
  meta(rt_iif eq 42)
filter parent 1: protocol all pref 1 basic chain 0 handle 0x2 flowid 1:b001
  meta(rt_iif eq 57)
filter parent 1: protocol all pref 1 basic chain 0 handle 0x3 flowid 1:000c
`

	assert.Equal(t, []bridgeFilter{
		{handle: "0x1", flowID: "1:a3f2", rtIif: 42},
		{handle: "0x2", flowID: "1:b001", rtIif: 57},
		{handle: "0x3", flowID: "1:000c", rtIif: -1},
	}, parseBridgeFilters(output))
}

func TestParseBridgeFiltersEmpty(t *testing.T) {
	assert.Empty(t, parseBridgeFilters(""))
}

func TestCountBridgeHTBClassesExcludesRoot(t *testing.T) {
	output := `class htb 1:1 root rate 100Mbit ceil 100Mbit burst 1600b cburst 1600b
class htb 1:a3f2 parent 1:1 leaf e001: prio 1 rate 1Mbit ceil 1Mbit burst 1600b cburst 1600b
class htb 1:b001 parent 1:1 leaf e002: prio 1 rate 1Mbit ceil 1Mbit burst 1600b cburst 1600b
`

	assert.Equal(t, int64(2), countBridgeHTBClasses(parseBridgeClasses(output)))
}

func TestPlanOrphanedBridgeTC(t *testing.T) {
	staleFilters, staleClasses, safe := planOrphanedBridgeTC(
		map[int]bool{42: true},
		[]bridgeFilter{
			{handle: "0x1", flowID: "1:a3f2", rtIif: 42},
			{handle: "0x2", flowID: "1:b001", rtIif: 57},
			{handle: "0x3", flowID: "1:000c", rtIif: -1},
		},
		[]string{"1:1", "1:a3f2", "1:b001", "1:000c", "1:9999"},
	)

	assert.True(t, safe)
	assert.Equal(t, []bridgeFilter{
		{handle: "0x2", flowID: "1:b001", rtIif: 57},
	}, staleFilters)
	assert.Equal(t, []string{"1:b001", "1:9999"}, staleClasses)
}

func TestPlanOrphanedBridgeTCBailsWhenNoRTIIFParses(t *testing.T) {
	staleFilters, staleClasses, safe := planOrphanedBridgeTC(
		map[int]bool{42: true},
		[]bridgeFilter{{handle: "0x1", flowID: "1:a3f2", rtIif: -1}},
		[]string{"1:a3f2"},
	)

	assert.False(t, safe)
	assert.Nil(t, staleFilters)
	assert.Nil(t, staleClasses)
}

func TestGuestFDBEntry(t *testing.T) {
	entry, err := guestFDBEntry(42, "02:00:00:aa:bb:cc")
	require.NoError(t, err)

	assert.Equal(t, 42, entry.LinkIndex)
	assert.Equal(t, unix.AF_BRIDGE, entry.Family)
	assert.Equal(t, netlink.NUD_PERMANENT, entry.State)
	assert.Equal(t, netlink.NTF_MASTER, entry.Flags)
	assert.Equal(t, "02:00:00:aa:bb:cc", entry.HardwareAddr.String())
	assert.Nil(t, entry.IP)
}

func TestGuestFDBEntryRejectsBadMAC(t *testing.T) {
	_, err := guestFDBEntry(42, "not-a-mac")
	assert.Error(t, err)
}

// TestHardenIsolatedPortOnRealBridge exercises the netlink calls against a real
// bridge, which is the only way to catch a request the kernel rejects.
func TestHardenIsolatedPortOnRealBridge(t *testing.T) {
	const mac = "02:00:00:aa:bb:cc"
	tap := bridgedTAPForTest(t, "brhardn0", "taphardn0")

	require.NoError(t, hardenIsolatedPort(context.Background(), tap, tap.Name, mac))

	protinfo, err := netlink.LinkGetProtinfo(tap)
	require.NoError(t, err)
	assert.False(t, protinfo.Flood, "unicast flooding should be off")
	assert.False(t, protinfo.Learning, "MAC learning should be off")

	entries, err := netlink.NeighList(tap.Attrs().Index, unix.AF_BRIDGE)
	require.NoError(t, err)
	pinned := slices.IndexFunc(entries, func(n netlink.Neigh) bool {
		return n.HardwareAddr.String() == mac
	})
	require.NotEqual(t, -1, pinned, "guest MAC should be pinned to the TAP port")
	assert.Equal(t, netlink.NUD_PERMANENT, entries[pinned].State)
}

// TestHardenIsolatedPortLeavesPortAloneOnBadMAC covers the case where the
// allocation carries a MAC we can't parse: the port keeps its defaults rather
// than ending up with flooding off and nothing pinned to it.
func TestHardenIsolatedPortLeavesPortAloneOnBadMAC(t *testing.T) {
	tap := bridgedTAPForTest(t, "brhardn1", "taphardn1")

	require.Error(t, hardenIsolatedPort(context.Background(), tap, tap.Name, ""))

	protinfo, err := netlink.LinkGetProtinfo(tap)
	require.NoError(t, err)
	assert.True(t, protinfo.Flood, "flooding should be untouched")
	assert.True(t, protinfo.Learning, "learning should be untouched")
}

// bridgedTAPForTest creates a throwaway bridge with one TAP enslaved to it, and
// skips the test when it can't (creating links needs root).
func bridgedTAPForTest(t *testing.T, bridgeName, tapName string) *netlink.Tuntap {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("Skipping test that requires root")
	}

	bridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: bridgeName}}
	require.NoError(t, netlink.LinkAdd(bridge))
	t.Cleanup(func() { _ = netlink.LinkDel(bridge) })

	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{Name: tapName},
		Mode:      netlink.TUNTAP_MODE_TAP,
	}
	require.NoError(t, netlink.LinkAdd(tap))
	t.Cleanup(func() { _ = netlink.LinkDel(tap) })
	require.NoError(t, netlink.LinkSetMaster(tap, bridge))

	return tap
}
