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
	Initialize(ctx context.Context, runningInstanceIDs []string) error

	// Instance allocation operations (called by instance manager)
	CreateAllocation(ctx context.Context, req AllocateRequest) (*NetworkConfig, error)
	RecreateAllocation(ctx context.Context, instanceID string) error
	ReleaseAllocation(ctx context.Context, alloc *Allocation) error

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

// Initialize initializes the network manager and creates default network.
// runningInstanceIDs should contain IDs of instances currently running (have active VMM).
func (m *manager) Initialize(ctx context.Context, runningInstanceIDs []string) error {
	log := logger.FromContext(ctx)

	// Derive gateway from subnet if not explicitly configured
	gateway := m.config.SubnetGateway
	if gateway == "" {
		var err error
		gateway, err = DeriveGateway(m.config.SubnetCIDR)
		if err != nil {
			return fmt.Errorf("derive gateway from subnet: %w", err)
		}
	}

	log.InfoContext(ctx, "initializing network manager",
		"bridge", m.config.BridgeName,
		"subnet", m.config.SubnetCIDR,
		"gateway", gateway)

	// Check for subnet conflicts with existing host routes before creating bridge
	if err := m.checkSubnetConflicts(ctx, m.config.SubnetCIDR); err != nil {
		return err
	}

	// Ensure default network bridge exists and iptables rules are configured
	// createBridge is idempotent - handles both new and existing bridges
	if err := m.createBridge(ctx, m.config.BridgeName, gateway, m.config.SubnetCIDR); err != nil {
		return fmt.Errorf("setup default network: %w", err)
	}

	// Cleanup orphaned TAP devices from previous runs (crashes, power loss, etc.)
	if deleted := m.CleanupOrphanedTAPs(ctx, runningInstanceIDs); deleted > 0 {
		log.InfoContext(ctx, "cleaned up orphaned TAP devices", "count", deleted)
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

