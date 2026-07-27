package egressproxy

// ApplyEnforcement installs host-side egress enforcement for an instance TAP
// device without registering the instance with the MITM proxy service. Used
// for enforcement-only egress policies (network.egress.proxy=none).
func ApplyEnforcement(instanceID, tapDevice, gatewayIP string, blockAllTCPEgress, blockUDPEgress bool) error {
	return applyEgressEnforcement(instanceID, tapDevice, gatewayIP, blockAllTCPEgress, blockUDPEgress)
}

// RemoveEnforcement removes any host-side egress enforcement rules for an
// instance. Safe to call for instances that never had enforcement applied.
func RemoveEnforcement(instanceID string) error {
	return removeEgressEnforcement(instanceID)
}
