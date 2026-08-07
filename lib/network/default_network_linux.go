//go:build linux

package network

import "fmt"

// platformDefaultNetwork returns the network requested by the Linux bridge
// configuration. Initialize verifies and creates this network before caching the
// bridge state reported by the kernel.
func (m *manager) platformDefaultNetwork() (*Network, error) {
	gateway := m.config.Network.SubnetGateway
	if gateway == "" {
		var err error
		gateway, err = DeriveGateway(m.config.Network.SubnetCIDR)
		if err != nil {
			return nil, fmt.Errorf("derive gateway from subnet: %w", err)
		}
	}

	return newDefaultNetwork(
		m.config.Network.BridgeName,
		m.config.Network.SubnetCIDR,
		gateway,
	), nil
}
