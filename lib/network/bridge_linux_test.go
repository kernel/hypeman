//go:build linux

package network

import (
	"os"
	"os/exec"
	"testing"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

func TestHTBUnknownCapacity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root and an HTB-capable kernel")
	}
	if os.Getenv("HYPEMAN_TEST_HTB_NAMESPACE") != "1" {
		cmd := exec.CommandContext(t.Context(), "unshare", "--net", os.Args[0], "-test.run=^TestHTBUnknownCapacity$", "-test.v")
		cmd.Env = append(os.Environ(), "HYPEMAN_TEST_HTB_NAMESPACE=1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		t.Logf("%s", output)
		return
	}

	ctx := t.Context()
	bridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "htb-test"}}
	require.NoError(t, netlink.LinkAdd(bridge))
	tap := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "htb-tap"}}
	require.NoError(t, netlink.LinkAdd(tap))
	tapLink, err := netlink.LinkByName(tap.Name)
	require.NoError(t, err)
	cfg := &config.Config{Network: config.NetworkConfig{BridgeName: bridge.Name}}
	show := func(t *testing.T, kind string) string {
		output, err := exec.Command("tc", kind, "show", "dev", bridge.Name).CombinedOutput()
		require.NoError(t, err, "%s", output)
		return string(output)
	}

	for _, tt := range []struct {
		name     string
		capacity int64
		rootRate string
	}{
		{"unknown", 0, ""},
		{"known-after-unknown", 125_000_000, "1Gbit"},
		{"unknown-after-known", 0, ""},
		{"updated-known", 250_000_000, "2Gbit"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := &manager{config: cfg}
			require.NoError(t, m.SetupHTB(ctx, tt.capacity))
			classID, err := m.addVMClass(ctx, bridge.Name, tap.Name, 37_500_000, 150_000_000)
			require.NoError(t, err)
			parent := "root"
			if tt.capacity > 0 {
				parent = "parent 1:1"
				assert.Contains(t, show(t, "class"), "class htb 1:1 root rate "+tt.rootRate)
			}
			classes := show(t, "class")
			assert.Contains(t, classes, "class htb 1:"+classID+" "+parent)
			assert.Contains(t, classes, "rate 300Mbit ceil 1200Mbit")
			if tt.name == "unknown" {
				assert.Len(t, parseBridgeClasses(classes), 1)
			}

			filters := show(t, "filter")
			m = &manager{config: cfg}
			require.NoError(t, m.SetupHTB(ctx, tt.capacity))
			assert.Equal(t, classes, show(t, "class"))
			assert.Equal(t, filters, show(t, "filter"))
			require.NoError(t, m.removeVMClass(ctx, bridge.Name, tapLink.Attrs().Index))
		})
	}
}

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
