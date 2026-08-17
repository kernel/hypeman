package network

// Model identifies the guest networking model a host provides. Each platform
// backend returns its model from NetworkModel; API layers map it onto their
// own wire enums so this package stays free of API dependencies.
type Model string

const (
	// ModelBridge is a Linux bridge with per-VM TAP devices.
	ModelBridge Model = "bridge"
	// ModelNAT is hypervisor-provided NAT (Virtualization.framework on macOS).
	ModelNAT Model = "nat"
)
