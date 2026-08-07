//go:build darwin

package network

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newDarwinTestManager(t *testing.T) *manager {
	t.Helper()
	cfg := &config.Config{
		Network: config.NetworkConfig{
			BridgeName:    "vmbr0",
			SubnetCIDR:    "10.100.0.0/16",
			SubnetGateway: "10.100.0.99",
			DNSServer:     "1.1.1.1",
		},
	}
	return NewManager(paths.New(t.TempDir()), cfg, nil).(*manager)
}

func TestDarwinEffectiveDefaultNetworkIgnoresLinuxConfig(t *testing.T) {
	m := newDarwinTestManager(t)

	beforeInitialize, err := m.EffectiveDefaultNetwork()
	require.NoError(t, err)
	assert.Equal(t, vzNATBridge, beforeInitialize.Bridge)
	assert.Equal(t, vzNATSubnet, beforeInitialize.Subnet)
	assert.Equal(t, vzNATGateway, beforeInitialize.Gateway)

	require.NoError(t, m.Initialize(context.Background(), nil))
	afterInitialize, err := m.EffectiveDefaultNetwork()
	require.NoError(t, err)
	assert.Equal(t, beforeInitialize, afterInitialize)
}

func TestDarwinAllocationAndDerivationUseVZNATNetwork(t *testing.T) {
	ctx := context.Background()
	m := newDarwinTestManager(t)
	require.NoError(t, m.Initialize(ctx, nil))

	const instanceID = "darwin-network-regression"
	networkConfig, err := m.CreateAllocation(ctx, AllocateRequest{
		InstanceID:   instanceID,
		InstanceName: "darwin-network-regression",
	})
	require.NoError(t, err)
	assert.Equal(t, vzNATGateway, networkConfig.Gateway)
	assert.Equal(t, "255.255.255.0", networkConfig.Netmask)
	assert.Equal(t, "1.1.1.1", networkConfig.DNS)
	_, vzSubnet, err := net.ParseCIDR(vzNATSubnet)
	require.NoError(t, err)
	assert.True(t, vzSubnet.Contains(net.ParseIP(networkConfig.IP)))

	metadata, err := json.Marshal(instanceMetadata{
		Name:           "darwin-network-regression",
		NetworkEnabled: true,
		HypervisorType: "vz",
		IP:             networkConfig.IP,
		MAC:            networkConfig.MAC,
	})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(m.paths.InstanceDir(instanceID), 0o755))
	require.NoError(t, os.WriteFile(m.paths.InstanceMetadata(instanceID), metadata, 0o644))

	allocation, err := m.GetAllocation(ctx, instanceID)
	require.NoError(t, err)
	require.NotNil(t, allocation)
	assert.Equal(t, vzNATGateway, allocation.Gateway)
	assert.Equal(t, "255.255.255.0", allocation.Netmask)
	assert.Equal(t, networkConfig.IP, allocation.IP)
}
