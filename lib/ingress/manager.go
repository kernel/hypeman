package ingress

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/nrednav/cuid2"
	"github.com/onkernel/hypeman/lib/dns"
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

	// Get retrieves an ingress by ID, name, or ID prefix.
	// Lookup order: exact ID match -> exact name match -> ID prefix match.
	// Returns ErrAmbiguousName if prefix matches multiple ingresses.
	Get(ctx context.Context, idOrName string) (*Ingress, error)

	// List returns all ingress resources.
	List(ctx context.Context) ([]Ingress, error)

	// Delete removes an ingress resource by ID, name, or ID prefix.
	// Lookup order: exact ID match -> exact name match -> ID prefix match.
	// Returns ErrAmbiguousName if prefix matches multiple ingresses.
	Delete(ctx context.Context, idOrName string) error

	// Shutdown gracefully stops the ingress subsystem.
	Shutdown() error
}

// DefaultDNSPort is the default port for the internal DNS server.
const DefaultDNSPort = dns.DefaultPort

// Config holds configuration for the ingress manager.
type Config struct {
	// ListenAddress is the address Caddy should listen on (default: 0.0.0.0).
	ListenAddress string

	// AdminAddress is the address for Caddy admin API (default: 127.0.0.1).
	AdminAddress string

	// AdminPort is the port for Caddy admin API (default: 2019).
	AdminPort int

	// DNSPort is the port for the internal DNS server used for dynamic upstream resolution.
	// Default: 5353. Set to 0 to use a random available port.
	DNSPort int

	// StopOnShutdown determines whether to stop Caddy when hypeman shuts down (default: false).
	// When false, Caddy continues running independently.
	StopOnShutdown bool

	// ACME configuration for TLS certificates
	ACME ACMEConfig
}

// DefaultConfig returns the default ingress configuration.
func DefaultConfig() Config {
	return Config{
		ListenAddress:  "0.0.0.0",
		AdminAddress:   "127.0.0.1",
		AdminPort:      2019,
		DNSPort:        dns.DefaultPort,
		StopOnShutdown: false,
	}
}

type manager struct {
	paths            *paths.Paths
	config           Config
	instanceResolver InstanceResolver
	daemon           *CaddyDaemon
	configGenerator  *CaddyConfigGenerator
	logForwarder     *CaddyLogForwarder
	dnsServer        *dns.Server
	mu               sync.RWMutex
}

// NewManager creates a new ingress manager.
// If otelLogger is non-nil, Caddy system logs will be forwarded to OTEL.
func NewManager(p *paths.Paths, config Config, instanceResolver InstanceResolver, otelLogger *slog.Logger) Manager {
	daemon := NewCaddyDaemon(p, config.AdminAddress, config.AdminPort, config.StopOnShutdown)

	// Create log forwarder if OTEL logger is provided
	var logForwarder *CaddyLogForwarder
	if otelLogger != nil {
		logForwarder = NewCaddyLogForwarder(p, otelLogger)
	}

	// Create DNS server for instance resolution
	// The InstanceResolver interface is compatible with dns.InstanceResolver
	dnsServer := dns.NewServer(instanceResolver, config.DNSPort, otelLogger)

	// Create config generator with initial DNS port
	// Note: If DNSPort was 0 (random), the actual port is determined in Initialize()
	// after the DNS server starts. The config generator is recreated there with the actual port.
	configGenerator := NewCaddyConfigGenerator(
		p,
		config.ListenAddress,
		config.AdminAddress,
		config.AdminPort,
		config.ACME,
		dnsServer.Port(),
	)

	return &manager{
		paths:            p,
		config:           config,
		instanceResolver: instanceResolver,
		daemon:           daemon,
		configGenerator:  configGenerator,
		logForwarder:     logForwarder,
		dnsServer:        dnsServer,
	}
}

// Initialize starts the ingress subsystem.
func (m *manager) Initialize(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	log := logger.FromContext(ctx)

	// Start DNS server for instance resolution
	if err := m.dnsServer.Start(ctx); err != nil {
		return fmt.Errorf("start DNS server: %w", err)
	}

	// Create config generator now that DNS server is running and we know the actual port
	// (important when DNSPort was configured as 0 for random port)
	m.configGenerator = NewCaddyConfigGenerator(
		m.paths,
		m.config.ListenAddress,
		m.config.AdminAddress,
		m.config.AdminPort,
		m.config.ACME,
		m.dnsServer.Port(),
	)

	// Load existing ingresses
	ingresses, err := m.loadAllIngresses()
	if err != nil {
		return fmt.Errorf("load ingresses: %w", err)
	}

	// Check if any TLS ingresses exist but TLS isn't configured
	if HasTLSRules(ingresses) && !m.config.ACME.IsTLSConfigured() {
		log.WarnContext(ctx, "TLS ingresses exist but ACME is not configured - TLS will not work")
	}

	// Generate and write config
	if err := m.regenerateConfig(ctx, ingresses); err != nil {
		return fmt.Errorf("regenerate config: %w", err)
	}

	// Start Caddy daemon
	_, err = m.daemon.Start(ctx)
	if err != nil {
		return fmt.Errorf("start caddy: %w", err)
	}

	// Start log forwarder (if configured) to forward Caddy system logs to OTEL
	if m.logForwarder != nil {
		if err := m.logForwarder.Start(ctx); err != nil {
			log.WarnContext(ctx, "failed to start caddy log forwarder", "error", err)
			// Non-fatal - continue without log forwarding
		}
	}

	return nil
}

// Create creates a new ingress resource.
func (m *manager) Create(ctx context.Context, req CreateIngressRequest) (*Ingress, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	log := logger.FromContext(ctx)

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

	// Check if TLS is requested but ACME isn't configured, and validate allowed domains
	for _, rule := range req.Rules {
		if rule.TLS {
			if !m.config.ACME.IsTLSConfigured() {
				return nil, fmt.Errorf("%w: TLS requested but ACME is not configured (set ACME_EMAIL and ACME_DNS_PROVIDER)", ErrInvalidRequest)
			}
			// Check if domain is in the allowed list
			// For pattern hostnames, check the wildcard pattern (e.g., "*.example.com")
			domainToCheck := rule.Match.Hostname
			if rule.Match.IsPattern() {
				pattern, err := rule.Match.ParsePattern()
				if err != nil {
					return nil, fmt.Errorf("invalid hostname pattern: %w", err)
				}
				domainToCheck = pattern.Wildcard
			}
			if !m.config.ACME.IsDomainAllowed(domainToCheck) {
				return nil, fmt.Errorf("%w: %q is not in TLS_ALLOWED_DOMAINS (allowed: %s)", ErrDomainNotAllowed, domainToCheck, m.config.ACME.AllowedDomains)
			}
		}
	}

	// Validate that all target instances exist (only for literal hostnames)
	// Pattern hostnames have dynamic target instances that can't be validated at creation time
	for _, rule := range req.Rules {
		if !rule.Match.IsPattern() {
			// Literal hostname - validate instance exists
			exists, err := m.instanceResolver.InstanceExists(ctx, rule.Target.Instance)
			if err != nil {
				return nil, fmt.Errorf("check instance %q: %w", rule.Target.Instance, err)
			}
			if !exists {
				return nil, fmt.Errorf("%w: instance %q not found", ErrInstanceNotFound, rule.Target.Instance)
			}
		}
		// For pattern hostnames, instance validation happens at request time via the upstream resolver
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

	// Generate config with the new ingress included
	// Use slices.Concat to avoid modifying the existingIngresses slice
	allIngresses := slices.Concat(existingIngresses, []Ingress{ingress})

	configData, err := m.configGenerator.GenerateConfig(ctx, allIngresses)
	if err != nil {
		return nil, fmt.Errorf("generate config: %w", err)
	}

	// Apply config to Caddy - this validates and applies atomically
	// If Caddy rejects the config, we don't persist the ingress
	if m.daemon.IsRunning() {
		if err := m.daemon.ReloadConfig(configData); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrConfigValidationFailed, err)
		}
	}

	// Config accepted - save ingress to storage
	stored := &storedIngress{
		ID:        ingress.ID,
		Name:      ingress.Name,
		Rules:     ingress.Rules,
		CreatedAt: ingress.CreatedAt.Format(time.RFC3339),
	}

	if err := saveIngress(m.paths, stored); err != nil {
		return nil, fmt.Errorf("save ingress: %w", err)
	}

	// Write config to disk (for Caddy restarts)
	if err := m.configGenerator.WriteConfig(ctx, allIngresses); err != nil {
		// Try to clean up the saved ingress
		deleteIngressData(m.paths, id)
		log.ErrorContext(ctx, "failed to write config after create", "error", err)
		return nil, fmt.Errorf("write config: %w", err)
	}

	return &ingress, nil
}

// Get retrieves an ingress by ID, name, or ID prefix.
// Lookup order: exact ID match -> exact name match -> ID prefix match.
// Returns ErrAmbiguousName if prefix matches multiple ingresses.
func (m *manager) Get(ctx context.Context, idOrName string) (*Ingress, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.resolveIngress(idOrName)
}

// resolveIngress finds an ingress by ID, name, or ID prefix.
// Must be called with at least a read lock held.
func (m *manager) resolveIngress(idOrName string) (*Ingress, error) {
	// 1. Try exact ID match first (most common case)
	stored, err := loadIngress(m.paths, idOrName)
	if err == nil {
		return storedToIngress(stored), nil
	}

	// 2. Load all ingresses for name and prefix matching
	allIngresses, err := loadAllIngresses(m.paths)
	if err != nil {
		return nil, err
	}

	// 3. Try exact name match
	var nameMatches []storedIngress
	for _, ing := range allIngresses {
		if ing.Name == idOrName {
			nameMatches = append(nameMatches, ing)
		}
	}
	if len(nameMatches) == 1 {
		return storedToIngress(&nameMatches[0]), nil
	}
	if len(nameMatches) > 1 {
		return nil, ErrAmbiguousName
	}

	// 4. Try ID prefix match
	var prefixMatches []storedIngress
	for _, ing := range allIngresses {
		if len(idOrName) > 0 && len(ing.ID) >= len(idOrName) && ing.ID[:len(idOrName)] == idOrName {
			prefixMatches = append(prefixMatches, ing)
		}
	}
	if len(prefixMatches) == 1 {
		return storedToIngress(&prefixMatches[0]), nil
	}
	if len(prefixMatches) > 1 {
		return nil, ErrAmbiguousName
	}

	return nil, ErrNotFound
}

// List returns all ingress resources.
func (m *manager) List(ctx context.Context) ([]Ingress, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.loadAllIngresses()
}

// Delete removes an ingress resource by ID, name, or ID prefix.
func (m *manager) Delete(ctx context.Context, idOrName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	log := logger.FromContext(ctx)

	// Find the ingress using ID/name/prefix resolution
	ingress, err := m.resolveIngress(idOrName)
	if err != nil {
		return err
	}
	id := ingress.ID

	// Delete from storage
	if err := deleteIngressData(m.paths, id); err != nil {
		return fmt.Errorf("delete ingress data: %w", err)
	}

	// Regenerate config without the deleted ingress
	ingresses, err := m.loadAllIngresses()
	if err != nil {
		return fmt.Errorf("load ingresses: %w", err)
	}

	// Generate and validate new config
	configData, err := m.configGenerator.GenerateConfig(ctx, ingresses)
	if err != nil {
		return fmt.Errorf("generate config: %w", err)
	}

	// Apply new config
	if m.daemon.IsRunning() {
		if err := m.daemon.ReloadConfig(configData); err != nil {
			log.ErrorContext(ctx, "failed to reload caddy config after delete", "error", err)
			return ErrConfigValidationFailed
		}
	}

	// Write config to disk
	if err := m.configGenerator.WriteConfig(ctx, ingresses); err != nil {
		log.ErrorContext(ctx, "failed to write config after delete", "error", err)
	}

	return nil
}

// Shutdown gracefully stops the ingress subsystem.
func (m *manager) Shutdown() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Stop log forwarder
	if m.logForwarder != nil {
		m.logForwarder.Stop()
	}

	// Stop DNS server
	if m.dnsServer != nil {
		if err := m.dnsServer.Stop(); err != nil {
			// Log but don't fail - continue with shutdown
			slog.Warn("failed to stop DNS server", "error", err)
		}
	}

	// Only stop Caddy if configured to do so
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

// regenerateConfig regenerates the Caddy config file from the given ingresses.
func (m *manager) regenerateConfig(ctx context.Context, ingresses []Ingress) error {
	return m.configGenerator.WriteConfig(ctx, ingresses)
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
