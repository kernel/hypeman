//go:build linux

package network

// NetworkModel identifies the guest networking model in use on this host.
// Linux hosts use a bridge with per-VM TAP devices.
func NetworkModel() Model {
	return ModelBridge
}

// GuestToGuestEnabled reports whether direct VM-to-VM traffic is permitted on
// the given network. Hypeman provisions its default networks with per-TAP
// port isolation (Isolated=true), which blocks direct guest-to-guest traffic.
// The flag is read from the network rather than assumed so a non-isolated
// network is reported truthfully.
func GuestToGuestEnabled(n *Network) bool {
	return n != nil && !n.Isolated
}
