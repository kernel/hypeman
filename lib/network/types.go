package network

import "time"

// Network represents a virtual network for instances
type Network struct {
	Name      string    // "default", "internal"
	Subnet    string    // "192.168.100.0/24"
	Gateway   string    // "192.168.100.1"
	Bridge    string    // "vmbr0" (derived from kernel)
	Isolated  bool      // Bridge_slave isolation mode
	DNSDomain string    // "hypeman"
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
type AllocateRequest struct {
	InstanceID   string
	InstanceName string
	Network      string // "default", "internal", or "" for no network
}

// CreateNetworkRequest is the request to create a new network
type CreateNetworkRequest struct {
	Name     string // Required, lowercase alphanumeric with dashes
	Subnet   string // Required, CIDR notation (e.g., "192.168.101.0/24")
	Isolated bool   // Optional, defaults to true
}

