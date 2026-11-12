//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
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

func TestRetryNetlinkDumpRetriesInterruptedDump(t *testing.T) {
	interrupts := 2
	calls := 0
	err := retryNetlinkDump(func() error {
		calls++
		if calls <= interrupts {
			return netlink.ErrDumpInterrupted
		}
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, interrupts+1, calls)
}

func TestRetryNetlinkDumpReturnsOtherErrorsImmediately(t *testing.T) {
	calls := 0
	errBoom := errors.New("boom")
	err := retryNetlinkDump(func() error {
		calls++
		return errBoom
	})

	require.ErrorIs(t, err, errBoom)
	assert.Equal(t, 1, calls)
}

func TestRetryNetlinkDumpGivesUpAfterBudget(t *testing.T) {
	calls := 0
	err := retryNetlinkDump(func() error {
		calls++
		return netlink.ErrDumpInterrupted
	})

	require.ErrorIs(t, err, netlink.ErrDumpInterrupted)
	assert.Equal(t, netlinkDumpRetryCount, calls)
}

func TestForgetNeighborDropsReusedAddressEntry(t *testing.T) {
	bridge := newNeighborTestBridge(t)
	ip := net.ParseIP("198.51.100.7").To4()
	addStaleNeighbor(t, bridge, ip)
	require.True(t, hasUsableNeighbor(t, bridge, ip))

	m := &manager{}
	require.NoError(t, m.forgetNeighbor(bridge.Name, ip.String()))
	assert.False(t, hasUsableNeighbor(t, bridge, ip))

	require.NoError(t, m.forgetNeighbor(bridge.Name, ip.String()), "a missing entry is not an error")
	assert.Error(t, m.forgetNeighbor("hm-no-such-bridge", ip.String()))
	assert.Error(t, m.forgetNeighbor(bridge.Name, "not-an-address"))
}

// A new guest must not inherit the neighbor entry of whichever guest held its
// address before: the host would keep sending to the old MAC.
func TestCreateAllocationForgetsNeighborOfReusedAddress(t *testing.T) {
	bridge := newNeighborTestBridge(t)
	const subnet = "198.51.100.0/29"
	var candidates []net.IP
	for last := byte(2); last <= 6; last++ {
		ip := net.IPv4(198, 51, 100, last).To4()
		candidates = append(candidates, ip)
		addStaleNeighbor(t, bridge, ip)
	}

	m := &manager{
		paths:              paths.New(t.TempDir()),
		config:             &config.Config{Network: config.NetworkConfig{BridgeName: bridge.Name}},
		pendingAllocations: make(map[string]pendingAllocation),
	}
	m.setDefaultNetwork(newDefaultNetwork(bridge.Name, subnet, "198.51.100.1"))

	// The TAP is named after the first eight characters of the instance ID.
	instanceID := fmt.Sprintf("n%07d", os.Getpid())
	allocated, err := m.CreateAllocation(context.Background(), AllocateRequest{InstanceID: instanceID, InstanceName: instanceID})
	require.NoError(t, err)
	t.Cleanup(func() {
		if tap, err := netlink.LinkByName(allocated.TAPDevice); err == nil {
			_ = netlink.LinkDel(tap)
		}
	})

	for _, ip := range candidates {
		assert.Equal(t, ip.String() != allocated.IP, hasUsableNeighbor(t, bridge, ip), "neighbor entry for %s", ip)
	}
}

func newNeighborTestBridge(t *testing.T) *netlink.Bridge {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("managing neighbor entries requires CAP_NET_ADMIN")
	}
	bridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: fmt.Sprintf("hmn%d", os.Getpid())}}
	require.NoError(t, netlink.LinkAdd(bridge))
	t.Cleanup(func() { _ = netlink.LinkDel(bridge) })

	// The kernel drops every neighbor entry of a bridge whose address changes
	// or that has no carrier when a port is added. Pin the address and give
	// the bridge a running port, like a host with guests has, so only the
	// entry under test goes away. The TAP has carrier while this process
	// holds it open.
	address, err := net.ParseMAC("02:00:00:00:00:01")
	require.NoError(t, err)
	require.NoError(t, netlink.LinkSetHardwareAddr(bridge, address))
	port := &netlink.Tuntap{
		LinkAttrs:  netlink.LinkAttrs{Name: fmt.Sprintf("hmp%d", os.Getpid())},
		Mode:       netlink.TUNTAP_MODE_TAP,
		Queues:     1,
		NonPersist: true,
	}
	require.NoError(t, netlink.LinkAdd(port))
	t.Cleanup(func() {
		for _, fd := range port.Fds {
			_ = fd.Close()
		}
	})
	require.NoError(t, netlink.LinkSetMaster(port, bridge))
	require.NoError(t, netlink.LinkSetUp(port))
	require.NoError(t, netlink.LinkSetUp(bridge))
	return bridge
}

func addStaleNeighbor(t *testing.T, bridge *netlink.Bridge, ip net.IP) {
	t.Helper()
	previousOwner, err := net.ParseMAC("02:00:00:aa:bb:cc")
	require.NoError(t, err)
	require.NoError(t, netlink.NeighSet(&netlink.Neigh{
		LinkIndex:    bridge.Attrs().Index,
		Family:       netlink.FAMILY_V4,
		State:        netlink.NUD_STALE,
		IP:           ip,
		HardwareAddr: previousOwner,
	}))
}

func hasUsableNeighbor(t *testing.T, bridge *netlink.Bridge, ip net.IP) bool {
	t.Helper()
	var neighbors []netlink.Neigh
	require.NoError(t, retryNetlinkDump(func() error {
		var err error
		neighbors, err = netlink.NeighList(bridge.Attrs().Index, netlink.FAMILY_V4)
		return err
	}))
	for _, neighbor := range neighbors {
		if neighbor.IP.Equal(ip) && neighbor.State != netlink.NUD_FAILED {
			return true
		}
	}
	return false
}
