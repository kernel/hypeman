package network

import "time"

// Network represents a virtual network for instances
type Network struct {
	Name      string    // "default", "internal"
	Subnet    string    // "192.168.0.0/16"
	Gateway   string    // "192.168.0.1"
	Bridge    string    // "vmbr0" (derived from kernel)
	Isolated  bool      // Bridge_slave isolation mode
	Default   bool      // True for default network
	CreatedAt time.Time
}

// Allocation represents a network allocation for an instance
type Allocation struct {
	InstanceID   string
	InstanceName string
	Network      string
	IP           string
	MAC          string
	TAPDevice    string
	Gateway      string // Gateway IP for this network
	Netmask      string // Netmask in dotted decimal notation
	State        string // "running", "standby" (derived from CH or snapshot)
}

// NetworkConfig is the configuration returned after allocation
type NetworkConfig struct {
	IP        string
	MAC       string
	Gateway   string
	Netmask   string
	DNS       string
	TAPDevice string
}

// AllocateRequest is the request to allocate network for an instance
// Always allocates from the default network
type AllocateRequest struct {
	InstanceID   string
	InstanceName string
}

