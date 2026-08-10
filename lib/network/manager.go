package network

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/paths"
	"go.opentelemetry.io/otel/metric"
)

// Manager defines the interface for network management
type Manager interface {
	// Lifecycle
	Initialize(ctx context.Context, runningInstanceIDs []string) error

	// Instance allocation operations (called by instance manager)
	CreateAllocation(ctx context.Context, req AllocateRequest) (*NetworkConfig, error)
	RecreateAllocation(ctx context.Context, instanceID string, downloadBps, uploadBps int64) error
	ReleaseAllocation(ctx context.Context, alloc *Allocation) error
	// ReleaseByInstanceID is a best-effort cleanup fallback when the full Allocation
	// can't be derived (e.g. metadata read failed). Deletes the TAP device using the
	// deterministic name from the instance ID.
	ReleaseByInstanceID(ctx context.Context, instanceID string) error

	// SetupHTB initializes HTB qdisc on the bridge for upload fair sharing.
	// Should be called during network initialization with the total network capacity.
	SetupHTB(ctx context.Context, capacityBps int64) error

	// Queries (derive from CH/snapshots)
	GetAllocation(ctx context.Context, instanceID string) (*Allocation, error)
	ListAllocations(ctx context.Context) ([]Allocation, error)
	NameExists(ctx context.Context, name string, excludeInstanceID string) (bool, error)
	// EffectiveDefaultNetwork returns the platform backend's default network.
	// Before Initialize it returns the network the backend will create or attach;
	// after Initialize it returns the backend state cached by the manager.
	EffectiveDefaultNetwork() (*Network, error)

	// CleanupOrphanedTAPs removes TAP devices not associated with any preserved
	// instance. Pass minAge>0 to skip TAPs younger than that, which avoids racing
	// against in-flight CreateAllocation calls whose metadata hasn't been persisted.
	CleanupOrphanedTAPs(ctx context.Context, preserveInstanceIDs []string, minAge time.Duration) int

	// CleanupOrphanedClasses removes bridge tc filters/classes not referenced by
	// a live TAP's filter.
	CleanupOrphanedClasses(ctx context.Context) int

	// GetUploadBurstMultiplier returns the configured multiplier for upload burst ceiling.
	GetUploadBurstMultiplier() int

	// GetDownloadBurstMultiplier returns the configured multiplier for download burst bucket.
	GetDownloadBurstMultiplier() int
}

// manager implements the Manager interface
type manager struct {
	paths              *paths.Paths
	config             *config.Config
	mu                 sync.Mutex // Protects network identity reservation.
	networkMu          sync.RWMutex
	defaultNetwork     *Network
	pendingAllocations map[string]pendingAllocation
	tcMu               sync.Mutex // Serializes shared bridge tc mutations.
	metrics            *Metrics
}

type pendingAllocation struct {
	allocation Allocation
}

// NewManager creates a new network manager.
// If meter is nil, metrics are disabled.
func NewManager(p *paths.Paths, cfg *config.Config, meter metric.Meter) Manager {
	m := &manager{
		paths:              p,
		config:             cfg,
		pendingAllocations: make(map[string]pendingAllocation),
	}

	// Initialize metrics if meter is provided
	if meter != nil {
		metrics, err := newNetworkMetrics(meter, m)
		if err == nil {
			m.metrics = metrics
		}
	}

	return m
}

// Initialize initializes the network manager and creates default network.
// runningInstanceIDs should contain IDs of instances currently running (have active VMM).
func (m *manager) Initialize(ctx context.Context, runningInstanceIDs []string) error {
	log := logger.FromContext(ctx)

	requestedNetwork, err := m.platformDefaultNetwork()
	if err != nil {
		return err
	}

	log.InfoContext(ctx, "initializing network manager",
		"bridge", requestedNetwork.Bridge,
		"subnet", requestedNetwork.Subnet,
		"gateway", requestedNetwork.Gateway)

	// Check for subnet conflicts with existing host routes before creating bridge.
	if err := m.checkSubnetConflicts(ctx, requestedNetwork.Subnet); err != nil {
		return err
	}

	// Ensure the platform backend is initialized. On Linux this creates the
	// configured bridge; on Darwin VZ supplies its own NAT network.
	if err := m.createBridge(ctx, requestedNetwork.Bridge, requestedNetwork.Gateway, requestedNetwork.Subnet); err != nil {
		return fmt.Errorf("setup default network: %w", err)
	}

	// The backend state is authoritative once initialization has completed. In
	// particular, VZ's NAT subnet must win over Linux-oriented config defaults.
	effectiveNetwork, err := m.getDefaultNetwork(ctx)
	if err != nil {
		return fmt.Errorf("get effective default network: %w", err)
	}
	m.setDefaultNetwork(effectiveNetwork)

	// Cleanup orphaned TAP devices from previous runs (crashes, power loss, etc.).
	// Startup runs before any concurrent CreateAllocation can be in flight, so no
	// age filter is needed here. The periodic reaper passes a non-zero minAge.
	if deleted := m.CleanupOrphanedTAPs(ctx, runningInstanceIDs, 0); deleted > 0 {
		log.InfoContext(ctx, "cleaned up orphaned TAP devices", "count", deleted)
	}

	// Cleanup orphaned HTB classes (TAPs deleted externally but classes remain)
	if deleted := m.CleanupOrphanedClasses(ctx); deleted > 0 {
		log.InfoContext(ctx, "cleaned up orphaned HTB classes", "count", deleted)
	}

	log.InfoContext(ctx, "network manager initialized")
	return nil
}

func cloneNetwork(network *Network) *Network {
	if network == nil {
		return nil
	}
	cloned := *network
	return &cloned
}

func (m *manager) cachedDefaultNetwork() *Network {
	m.networkMu.RLock()
	defer m.networkMu.RUnlock()
	return cloneNetwork(m.defaultNetwork)
}

func (m *manager) setDefaultNetwork(network *Network) {
	m.networkMu.Lock()
	defer m.networkMu.Unlock()
	m.defaultNetwork = cloneNetwork(network)
}

func newDefaultNetwork(bridge, subnet, gateway string) *Network {
	return &Network{
		Name:     "default",
		Subnet:   subnet,
		Gateway:  gateway,
		Bridge:   bridge,
		Isolated: true,
		Default:  true,
	}
}

// EffectiveDefaultNetwork returns one platform-authoritative network identity.
// The pre-initialization value is useful to dependency providers that need the
// guest-visible gateway before the manager's startup initialization runs.
func (m *manager) EffectiveDefaultNetwork() (*Network, error) {
	if network := m.cachedDefaultNetwork(); network != nil {
		return network, nil
	}
	return m.platformDefaultNetwork()
}

// getDefaultNetwork gets the default network details from backend state.
func (m *manager) getDefaultNetwork(ctx context.Context) (*Network, error) {
	state, err := m.queryNetworkState(m.config.Network.BridgeName)
	if err != nil {
		return nil, fmt.Errorf("query default network state: %w", err)
	}

	bridge := state.Bridge
	if bridge == "" {
		bridge = m.config.Network.BridgeName
	}
	network := newDefaultNetwork(bridge, state.Subnet, state.Gateway)
	network.CreatedAt = time.Time{} // Unknown for default
	return network, nil
}

// SetupHTB initializes HTB qdisc on the bridge for upload fair sharing.
// capacityBps is the total network capacity in bytes per second.
func (m *manager) SetupHTB(ctx context.Context, capacityBps int64) error {
	return m.setupBridgeHTB(ctx, m.config.Network.BridgeName, capacityBps)
}

// GetUploadBurstMultiplier returns the configured multiplier for upload burst ceiling.
// Defaults to 4 if not configured.
func (m *manager) GetUploadBurstMultiplier() int {
	if m.config.Network.UploadBurstMultiplier < 1 {
		return DefaultUploadBurstMultiplier
	}
	return m.config.Network.UploadBurstMultiplier
}

// GetDownloadBurstMultiplier returns the configured multiplier for download burst bucket.
// Defaults to 4 if not configured.
func (m *manager) GetDownloadBurstMultiplier() int {
	if m.config.Network.DownloadBurstMultiplier < 1 {
		return DefaultDownloadBurstMultiplier
	}
	return m.config.Network.DownloadBurstMultiplier
}
