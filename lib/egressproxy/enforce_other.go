//go:build !linux

package egressproxy

func applyEgressEnforcement(instanceID, tapDevice, gatewayIP string, proxyPort int) error {
	return nil
}

func removeEgressEnforcement(instanceID string) error {
	return nil
}
