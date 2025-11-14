package network

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/paths"
)

// Manager defines the interface for network management
type Manager interface {
	// Lifecycle
	Initialize(ctx context.Context) error

	// Instance network operations (called by instance manager)
	AllocateNetwork(ctx context.Context, req AllocateRequest) (*NetworkConfig, error)
	RecreateNetwork(ctx context.Context, instanceID string) error
	ReleaseNetwork(ctx context.Context, alloc *Allocation) error

	// Queries (derive from CH/snapshots)
	GetAllocation(ctx context.Context, instanceID string) (*Allocation, error)
	ListAllocations(ctx context.Context) ([]Allocation, error)
	NameExists(ctx context.Context, name string) (bool, error)
}

// manager implements the Manager interface
type manager struct {
	paths  *paths.Paths
	config *config.Config
	mu     sync.Mutex // Protects network allocation operations (IP allocation)
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

	log.InfoContext(ctx, "network manager initialized")
	return nil
}

// getDefaultNetwork gets the default network details from kernel state
func (m *manager) getDefaultNetwork(ctx context.Context) (*Network, error) {
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
		Default:   true,
		CreatedAt: time.Time{}, // Unknown for default
	}, nil
}

