//go:build linux

package network

import (
	"context"
	"net"
	"testing"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

func TestLinuxEffectiveDefaultNetworkUsesBridgeConfig(t *testing.T) {
	cfg := &config.Config{
		Network: config.NetworkConfig{
			BridgeName:    "testbr0",
			SubnetCIDR:    "10.123.0.0/16",
			SubnetGateway: "10.123.0.42",
		},
	}
	m := NewManager(paths.New(t.TempDir()), cfg, nil)

	effective, err := m.EffectiveDefaultNetwork()
	require.NoError(t, err)
	assert.Equal(t, "testbr0", effective.Bridge)
	assert.Equal(t, "10.123.0.0/16", effective.Subnet)
	assert.Equal(t, "10.123.0.42", effective.Gateway)
}

func TestLinuxEffectiveDefaultNetworkDerivesGateway(t *testing.T) {
	cfg := &config.Config{
		Network: config.NetworkConfig{
			BridgeName: "testbr0",
			SubnetCIDR: "10.124.0.0/16",
		},
	}
	m := NewManager(paths.New(t.TempDir()), cfg, nil)

	effective, err := m.EffectiveDefaultNetwork()
	require.NoError(t, err)
	assert.Equal(t, "10.124.0.1", effective.Gateway)
}

func TestSelectBridgeGatewayAddrPrefersConfiguredGateway(t *testing.T) {
	addrs := []netlink.Addr{
		{IPNet: &net.IPNet{IP: net.ParseIP("10.123.0.2"), Mask: net.CIDRMask(16, 32)}},
		{IPNet: &net.IPNet{IP: net.ParseIP("10.123.0.1"), Mask: net.CIDRMask(16, 32)}},
	}

	selected := selectBridgeGatewayAddr(addrs, net.ParseIP("10.123.0.1"))
	assert.Equal(t, "10.123.0.1", selected.IP.String())
}

func TestCanonicalSubnetCIDRUsesNetworkAddress(t *testing.T) {
	assert.Equal(t, "10.123.0.0/16", canonicalSubnetCIDR(&net.IPNet{
		IP:   net.ParseIP("10.123.0.42"),
		Mask: net.CIDRMask(16, 32),
	}))
}

func TestBridgeAddressMatchesRequiresGatewayPrefix(t *testing.T) {
	addrs := []netlink.Addr{
		{IPNet: &net.IPNet{IP: net.ParseIP("10.123.0.1"), Mask: net.CIDRMask(16, 32)}},
	}

	hasGateway, hasMatchingMask := bridgeAddressMatches(addrs, net.ParseIP("10.123.0.1"), net.CIDRMask(24, 32))
	assert.True(t, hasGateway)
	assert.False(t, hasMatchingMask)

	hasGateway, hasMatchingMask = bridgeAddressMatches(addrs, net.ParseIP("10.123.0.1"), net.CIDRMask(16, 32))
	assert.True(t, hasGateway)
	assert.True(t, hasMatchingMask)
}

func TestGetDefaultNetworkPreservesLookupError(t *testing.T) {
	cfg := &config.Config{
		Network: config.NetworkConfig{BridgeName: "hypeman-no-such-bridge"},
	}
	m := NewManager(paths.New(t.TempDir()), cfg, nil).(*manager)

	_, err := m.getDefaultNetwork(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorContains(t, err, "query default network state")
	assert.ErrorContains(t, err, "look up bridge")
}
