//go:build !linux

package egressproxy

import "fmt"

func applyEgressEnforcement(instanceID, tapDevice, gatewayIP string, proxyPort int, blockAllTCPEgress bool) error {
	// Mediated mode installs no all-TCP block, so there is nothing to apply here.
	if !blockAllTCPEgress {
		return nil
	}
	// Blocking all TCP needs a host firewall rule; without one, refuse rather
	// than report enforcement that does not exist.
	return fmt.Errorf("egress enforcement is not implemented on this platform: no host firewall rule can be installed, so network.egress cannot be enforced")
}

func removeEgressEnforcement(instanceID string) error {
	_ = instanceID
	return nil
}
