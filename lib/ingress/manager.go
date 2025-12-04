package ingress

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/nrednav/cuid2"
	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/paths"
)

// InstanceResolver provides instance resolution capabilities.
// This interface is implemented by the instance manager.
type InstanceResolver interface {
	// ResolveInstanceIP resolves an instance name or ID to its IP address.
	// Returns the IP address and nil error if found, or an error if the instance
	// doesn't exist, isn't running, or has no network.
	ResolveInstanceIP(ctx context.Context, nameOrID string) (string, error)

	// InstanceExists checks if an instance with the given name or ID exists.
	InstanceExists(ctx context.Context, nameOrID string) (bool, error)
}

// Manager is the interface for managing ingress resources.
type Manager interface {
	// Initialize starts the ingress subsystem.
	// This should be called during server startup.
	Initialize(ctx context.Context) error

	// Create creates a new ingress resource.
	Create(ctx context.Context, req CreateIngressRequest) (*Ingress, error)

	// Get retrieves an ingress by ID or name.
	Get(ctx context.Context, idOrName string) (*Ingress, error)

	// List returns all ingress resources.
	List(ctx context.Context) ([]Ingress, error)

	// Delete removes an ingress resource.
	Delete(ctx context.Context, idOrName string) error

	// Shutdown gracefully stops the ingress subsystem.
	Shutdown() error
}

// Config holds configuration for the ingress manager.
type Config struct {
	// ListenAddress is the address Envoy should listen on (default: 0.0.0.0).
	ListenAddress string

	// AdminAddress is the address for Envoy admin API (default: 127.0.0.1).
	AdminAddress string

	// AdminPort is the port for Envoy admin API (default: 9901).
	AdminPort int

	// StopOnShutdown determines whether to stop Envoy when hypeman shuts down (default: false).
	// When false, Envoy continues running independently.
	StopOnShutdown bool

	// DisableValidation disables Envoy config validation before applying.
	// This should only be used for testing.
	DisableValidation bool

	// OTEL configuration for Envoy tracing
	OTEL OTELConfig
}

// OTELConfig holds OpenTelemetry configuration for Envoy.
type OTELConfig struct {
	// Enabled controls whether OTEL tracing is enabled in Envoy.
	Enabled bool

	// Endpoint is the OTEL collector gRPC endpoint (host:port).
	Endpoint string

	// ServiceName is the service name for traces (default: "hypeman-envoy").
	ServiceName string

	// ServiceInstanceID is the service instance identifier.
	ServiceInstanceID string

	// Insecure disables TLS for OTEL connections.
	Insecure bool

	// Environment is the deployment environment (e.g., dev, staging, prod).
	Environment string
}

// DefaultConfig returns the default ingress configuration.
func DefaultConfig() Config {
	return Config{
		ListenAddress:  "0.0.0.0",
		AdminAddress:   "127.0.0.1",
		AdminPort:      9901,
		StopOnShutdown: false,
	}
}

type manager struct {
	paths            *paths.Paths
	config           Config
	instanceResolver InstanceResolver
	daemon           *EnvoyDaemon
	configGenerator  *EnvoyConfigGenerator
	mu               sync.RWMutex
}

// NewManager creates a new ingress manager.
func NewManager(p *paths.Paths, config Config, instanceResolver InstanceResolver) Manager {
	daemon := NewEnvoyDaemon(p, config.AdminAddress, config.AdminPort, config.StopOnShutdown)

	// Use daemon as validator unless validation is disabled
	var validator ConfigValidator
	if !config.DisableValidation {
		validator = daemon
	}

	return &manager{
		paths:            p,
		config:           config,
		instanceResolver: instanceResolver,
		daemon:           daemon,
		configGenerator:  NewEnvoyConfigGenerator(p, config.ListenAddress, config.AdminAddress, config.AdminPort, validator, config.OTEL),
	}
}

// Initialize starts the ingress subsystem.
func (m *manager) Initialize(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Load existing ingresses and regenerate config
	ingresses, err := m.loadAllIngresses()
	if err != nil {
		return fmt.Errorf("load ingresses: %w", err)
	}

	// Generate and write config
	if err := m.regenerateConfig(ctx, ingresses); err != nil {
		return fmt.Errorf("regenerate config: %w", err)
	}

	// Start Envoy daemon
	_, err = m.daemon.Start(ctx)
	if err != nil {
		return fmt.Errorf("start envoy: %w", err)
	}

	return nil
}

// Create creates a new ingress resource.
func (m *manager) Create(ctx context.Context, req CreateIngressRequest) (*Ingress, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Validate request
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	// Validate name format
	if !isValidName(req.Name) {
		return nil, fmt.Errorf("%w: name must be lowercase letters, digits, and dashes only; cannot start or end with a dash", ErrInvalidRequest)
	}

	// Check if name already exists
	if _, err := findIngressByName(m.paths, req.Name); err == nil {
		return nil, fmt.Errorf("%w: ingress with name %q already exists", ErrAlreadyExists, req.Name)
	}

	// Validate that all target instances exist
	for _, rule := range req.Rules {
		exists, err := m.instanceResolver.InstanceExists(ctx, rule.Target.Instance)
		if err != nil {
			return nil, fmt.Errorf("check instance %q: %w", rule.Target.Instance, err)
		}
		if !exists {
			return nil, fmt.Errorf("%w: instance %q not found", ErrInstanceNotFound, rule.Target.Instance)
		}
	}

	// Check for hostname conflicts (hostname + port must be unique)
	existingIngresses, err := m.loadAllIngresses()
	if err != nil {
		return nil, fmt.Errorf("load existing ingresses: %w", err)
	}

	for _, rule := range req.Rules {
		newPort := rule.Match.GetPort()
		for _, existing := range existingIngresses {
			for _, existingRule := range existing.Rules {
				existingPort := existingRule.Match.GetPort()
				if existingRule.Match.Hostname == rule.Match.Hostname && existingPort == newPort {
					return nil, fmt.Errorf("%w: hostname %q on port %d is already used by ingress %q", ErrHostnameInUse, rule.Match.Hostname, newPort, existing.Name)
				}
			}
		}
	}

	// Generate ID
	id := cuid2.Generate()

	// Create ingress
	ingress := Ingress{
		ID:        id,
		Name:      req.Name,
		Rules:     req.Rules,
		CreatedAt: time.Now().UTC(),
	}

	// Save to storage
	stored := &storedIngress{
		ID:        ingress.ID,
		Name:      ingress.Name,
		Rules:     ingress.Rules,
		CreatedAt: ingress.CreatedAt.Format(time.RFC3339),
	}

	if err := saveIngress(m.paths, stored); err != nil {
		return nil, fmt.Errorf("save ingress: %w", err)
	}

	// Regenerate Envoy config with new ingress
	allIngresses := append(existingIngresses, ingress)
	if err := m.regenerateConfig(ctx, allIngresses); err != nil {
		// Try to clean up the saved ingress
		deleteIngressData(m.paths, id)
		return nil, fmt.Errorf("regenerate config: %w", err)
	}

	// Reload Envoy
	if m.daemon.IsRunning() {
		if err := m.daemon.ReloadConfig(); err != nil {
			log := logger.FromContext(ctx)
			log.ErrorContext(ctx, "failed to reload envoy config after create", "error", err)
			// Try to clean up the saved ingress since reload failed
			deleteIngressData(m.paths, id)
			return nil, ErrConfigValidationFailed
		}
	}

	return &ingress, nil
}

// Get retrieves an ingress by ID or name.
func (m *manager) Get(ctx context.Context, idOrName string) (*Ingress, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Try by ID first
	stored, err := loadIngress(m.paths, idOrName)
	if err == nil {
		return storedToIngress(stored), nil
	}

	// Try by name
	stored, err = findIngressByName(m.paths, idOrName)
	if err != nil {
		return nil, ErrNotFound
	}

	return storedToIngress(stored), nil
}

// List returns all ingress resources.
func (m *manager) List(ctx context.Context) ([]Ingress, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.loadAllIngresses()
}

// Delete removes an ingress resource.
func (m *manager) Delete(ctx context.Context, idOrName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Find the ingress
	var id string
	stored, err := loadIngress(m.paths, idOrName)
	if err == nil {
		id = stored.ID
	} else {
		// Try by name
		stored, err = findIngressByName(m.paths, idOrName)
		if err != nil {
			return ErrNotFound
		}
		id = stored.ID
	}

	// Delete from storage
	if err := deleteIngressData(m.paths, id); err != nil {
		return fmt.Errorf("delete ingress data: %w", err)
	}

	// Regenerate config without the deleted ingress
	ingresses, err := m.loadAllIngresses()
	if err != nil {
		return fmt.Errorf("load ingresses: %w", err)
	}

	if err := m.regenerateConfig(ctx, ingresses); err != nil {
		return fmt.Errorf("regenerate config: %w", err)
	}

	// Reload Envoy
	if m.daemon.IsRunning() {
		if err := m.daemon.ReloadConfig(); err != nil {
			log := logger.FromContext(ctx)
			log.ErrorContext(ctx, "failed to reload envoy config after delete", "error", err)
			return ErrConfigValidationFailed
		}
	}

	return nil
}

// Shutdown gracefully stops the ingress subsystem.
func (m *manager) Shutdown() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Only stop Envoy if configured to do so
	if m.daemon.StopOnShutdown() {
		return m.daemon.Stop()
	}

	return nil
}

// loadAllIngresses loads all ingresses and converts them to the Ingress type.
func (m *manager) loadAllIngresses() ([]Ingress, error) {
	storedList, err := loadAllIngresses(m.paths)
	if err != nil {
		return nil, err
	}

	ingresses := make([]Ingress, 0, len(storedList))
	for _, stored := range storedList {
		ingresses = append(ingresses, *storedToIngress(&stored))
	}

	return ingresses, nil
}

// regenerateConfig regenerates the Envoy config file from the given ingresses.
func (m *manager) regenerateConfig(ctx context.Context, ingresses []Ingress) error {
	ipResolver := func(instance string) (string, error) {
		return m.instanceResolver.ResolveInstanceIP(ctx, instance)
	}

	return m.configGenerator.WriteConfig(ctx, ingresses, ipResolver)
}

// storedToIngress converts a storedIngress to an Ingress.
func storedToIngress(stored *storedIngress) *Ingress {
	createdAt, _ := time.Parse(time.RFC3339, stored.CreatedAt)
	return &Ingress{
		ID:        stored.ID,
		Name:      stored.Name,
		Rules:     stored.Rules,
		CreatedAt: createdAt,
	}
}

// isValidName validates that a name matches the allowed pattern.
var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func isValidName(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}
	return namePattern.MatchString(name)
}
