//go:build darwin

package network

// NetworkModel identifies the guest networking model in use on this host.
// macOS hosts use Virtualization.framework-provided NAT.
func NetworkModel() Model {
	return ModelNAT
}

// GuestToGuestEnabled reports whether direct VM-to-VM traffic is permitted on
// the given network. Each vz guest sits behind its own NAT context, so direct
// guest-to-guest reachability is never provided regardless of the network's
// isolation flag.
func GuestToGuestEnabled(_ *Network) bool {
	return false
}
