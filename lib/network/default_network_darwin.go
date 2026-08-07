//go:build darwin

package network

const (
	vzNATBridge  = "nat"
	vzNATSubnet  = "192.168.64.0/24"
	vzNATGateway = "192.168.64.1"
)

// platformDefaultNetwork returns the default vmnet shared-NAT network targeted
// by Hypeman. Linux bridge settings do not change the network attached to VZ
// guests; host-level Shared_Net_Address overrides are not currently supported.
func (m *manager) platformDefaultNetwork() (*Network, error) {
	return newDefaultNetwork(vzNATBridge, vzNATSubnet, vzNATGateway), nil
}
