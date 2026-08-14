//go:build darwin

package network

// NetworkModel identifies the guest networking model in use on this host.
// macOS hosts use Virtualization.framework-provided NAT.
func NetworkModel() string {
	return "nat"
}

// GuestToGuestEnabled reports whether direct VM-to-VM traffic is permitted on
// the given default network. Each vz guest sits behind its own NAT context,
// so direct guest-to-guest reachability is not provided.
func GuestToGuestEnabled(_ *Network) bool {
	return false
}
