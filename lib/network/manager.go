package network

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"time"

	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/paths"
)

// Manager defines the interface for network management
type Manager interface {
	// Lifecycle
	Initialize(ctx context.Context) error

	// Network CRUD (default network cannot be deleted)
	CreateNetwork(ctx context.Context, req CreateNetworkRequest) (*Network, error)
	GetNetwork(ctx context.Context, name string) (*Network, error)
	ListNetworks(ctx context.Context) ([]Network, error)
	DeleteNetwork(ctx context.Context, name string) error

	// Instance network operations (called by instance manager)
	AllocateNetwork(ctx context.Context, req AllocateRequest) (*NetworkConfig, error)
	RecreateNetwork(ctx context.Context, instanceID string) error
	ReleaseNetwork(ctx context.Context, instanceID string) error

	// Queries (derive from CH/snapshots)
	GetAllocation(ctx context.Context, instanceID string) (*Allocation, error)
	ListAllocations(ctx context.Context) ([]Allocation, error)
	NameExistsInNetwork(ctx context.Context, name, network string) (bool, error)
}

// manager implements the Manager interface
// TODO @sjmiller609 review: Do we need some locks for possible race conditions managing networks?
type manager struct {
	paths  *paths.Paths
	config *config.Config
}

// NewManager creates a new network manager
func NewManager(p *paths.Paths, cfg *config.Config) Manager {
	return &manager{
		paths:  p,
		config: cfg,
	}
}

// Initialize initializes the network manager and creates default network
func (m *manager) Initialize(ctx context.Context) error {
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "initializing network manager")

	// 1. Check if default network bridge exists
	bridge := m.config.BridgeName
	_, err := m.queryNetworkState(bridge)
	if err != nil {
		// Default network doesn't exist, create it
		log.InfoContext(ctx, "creating default network",
			"bridge", bridge,
			"subnet", m.config.SubnetCIDR,
			"gateway", m.config.SubnetGateway)

		if err := m.createBridge(bridge, m.config.SubnetGateway, m.config.SubnetCIDR); err != nil {
			return fmt.Errorf("create default network bridge: %w", err)
		}
	} else {
		log.InfoContext(ctx, "default network already exists", "bridge", bridge)
	}

	// 2. Start dnsmasq
	if err := m.startDNS(ctx); err != nil {
		return fmt.Errorf("start DNS: %w", err)
	}

	log.InfoContext(ctx, "network manager initialized")
	return nil
}

// CreateNetwork creates a new network
func (m *manager) CreateNetwork(ctx context.Context, req CreateNetworkRequest) (*Network, error) {
	log := logger.FromContext(ctx)

	// 1. Validate network name
	if err := validateNetworkName(req.Name); err != nil {
		return nil, err
	}

	// 2. Check if network already exists
	if _, err := m.GetNetwork(ctx, req.Name); err == nil {
		return nil, fmt.Errorf("%w: network '%s' already exists", ErrAlreadyExists, req.Name)
	}

	// 3. Validate and parse subnet
	if _, _, err := net.ParseCIDR(req.Subnet); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSubnet, err)
	}

	// 4. Check for subnet overlap
	networks, err := m.ListNetworks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}

	for _, network := range networks {
		if subnetsOverlap(req.Subnet, network.Subnet) {
			return nil, fmt.Errorf("%w: overlaps with network '%s'", ErrSubnetOverlap, network.Name)
		}
	}

	// 5. Generate bridge name (vmbr0, vmbr1, etc.)
	bridgeName := m.generateBridgeName(networks)

	// 6. Calculate gateway IP (first IP in subnet)
	gateway, err := getFirstIP(req.Subnet)
	if err != nil {
		return nil, fmt.Errorf("calculate gateway: %w", err)
	}

	// 7. Create bridge
	if err := m.createBridge(bridgeName, gateway, req.Subnet); err != nil {
		return nil, fmt.Errorf("create bridge: %w", err)
	}

	// 8. Reload DNS to add new listen address
	if err := m.generateDNSConfig(ctx); err != nil {
		return nil, fmt.Errorf("update DNS config: %w", err)
	}
	if err := m.reloadDNS(ctx); err != nil {
		return nil, fmt.Errorf("reload DNS: %w", err)
	}

	network := &Network{
		Name:      req.Name,
		Subnet:    req.Subnet,
		Gateway:   gateway,
		Bridge:    bridgeName,
		Isolated:  req.Isolated,
		DNSDomain: "hypeman",
		Default:   false,
		CreatedAt: time.Now(),
	}

	log.InfoContext(ctx, "created network",
		"name", req.Name,
		"subnet", req.Subnet,
		"bridge", bridgeName)

	return network, nil
}

// GetNetwork gets a network by name
func (m *manager) GetNetwork(ctx context.Context, name string) (*Network, error) {
	// Special handling for default network
	if name == "default" {
		// Query from kernel
		state, err := m.queryNetworkState(m.config.BridgeName)
		if err != nil {
			return nil, ErrNotFound
		}

		return &Network{
			Name:      "default",
			Subnet:    state.Subnet,
			Gateway:   state.Gateway,
			Bridge:    m.config.BridgeName,
			Isolated:  true,
			DNSDomain: "hypeman",
			Default:   true,
			CreatedAt: time.Time{}, // Unknown for default
		}, nil
	}

	// For custom networks, we need to scan for bridges
	// For now, return not found - custom networks not fully implemented
	// (would need to persist network metadata)
	return nil, ErrNotFound
}

// ListNetworks lists all networks
func (m *manager) ListNetworks(ctx context.Context) ([]Network, error) {
	networks := []Network{}

	// Always include default network if it exists
	if defaultNet, err := m.GetNetwork(ctx, "default"); err == nil {
		networks = append(networks, *defaultNet)
	}

	// TODO: Scan for custom networks (would need persistence)

	return networks, nil
}

// DeleteNetwork deletes a network
func (m *manager) DeleteNetwork(ctx context.Context, name string) error {
	log := logger.FromContext(ctx)

	// 1. Check if default network
	if name == "default" {
		return fmt.Errorf("%w: cannot delete default network", ErrDefaultNetwork)
	}

	// 2. Get network
	network, err := m.GetNetwork(ctx, name)
	if err != nil {
		return err
	}

	// 3. Check for active instances
	allocations, err := m.ListAllocations(ctx)
	if err != nil {
		return fmt.Errorf("list allocations: %w", err)
	}

	for _, alloc := range allocations {
		if alloc.Network == name {
			return fmt.Errorf("%w: instance '%s' is using this network", ErrNetworkInUse, alloc.InstanceName)
		}
	}

	// 4. Delete bridge
	// (Not implemented for now - would use netlink.LinkDel)
	log.InfoContext(ctx, "delete network", "name", name, "bridge", network.Bridge)

	return fmt.Errorf("network deletion not yet implemented")
}

// validateNetworkName validates network name
func validateNetworkName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: name cannot be empty", ErrInvalidName)
	}

	// Must be lowercase alphanumeric with dashes
	// Cannot start or end with dash
	pattern := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	if !pattern.MatchString(name) {
		return fmt.Errorf("%w: must contain only lowercase letters, digits, and dashes; cannot start or end with dash", ErrInvalidName)
	}

	if len(name) > 63 {
		return fmt.Errorf("%w: name must be 63 characters or less", ErrInvalidName)
	}

	return nil
}

// subnetsOverlap checks if two subnets overlap
// TODO @sjmiller609 review: seems like we should allow different networks to overlap, check on this
func subnetsOverlap(subnet1, subnet2 string) bool {
	_, ipNet1, err := net.ParseCIDR(subnet1)
	if err != nil {
		return false
	}

	_, ipNet2, err := net.ParseCIDR(subnet2)
	if err != nil {
		return false
	}

	// Check if either subnet contains the other's network address
	return ipNet1.Contains(ipNet2.IP) || ipNet2.Contains(ipNet1.IP)
}

// getFirstIP gets the first IP in a subnet (for gateway)
func getFirstIP(subnet string) (string, error) {
	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return "", err
	}

	// Increment network address by 1 to get first usable IP
	ip := incrementIP(ipNet.IP, 1)
	return ip.String(), nil
}

// generateBridgeName generates a bridge name based on existing networks
func (m *manager) generateBridgeName(networks []Network) string {
	// Default is vmbr0, next would be vmbr1, etc.
	usedNumbers := make(map[int]bool)

	for _, network := range networks {
		// Parse bridge name (e.g., vmbr0 -> 0)
		var num int
		if _, err := fmt.Sscanf(network.Bridge, "vmbr%d", &num); err == nil {
			usedNumbers[num] = true
		}
	}

	// Find first unused number
	// TODO @sjmiller609 review: what is the max number of networks we can have and why?
	for i := 0; i < 100; i++ {
		if !usedNumbers[i] {
			return fmt.Sprintf("vmbr%d", i)
		}
	}

	// Fallback (shouldn't happen)
	return "vmbr99"
}

