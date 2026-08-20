//go:build !linux

package egressproxy

func applyEgressEnforcement(instanceID, sourceIP, tapDevice, gatewayIP string, proxyPort int, blockAllTCPEgress bool) error {
	_ = blockAllTCPEgress
	return nil
}

func removeEgressEnforcement(instanceID string) error {
	return nil
}
