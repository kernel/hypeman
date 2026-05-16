package instances

import (
	"context"
	"fmt"

	"github.com/kernel/hypeman/lib/healthcheck"
)

func cloneHealthCheckPolicy(policy *healthcheck.Policy) *healthcheck.Policy {
	return healthcheck.ClonePolicy(policy)
}

func normalizeHealthCheckPolicy(policy *healthcheck.Policy) (*healthcheck.Policy, error) {
	normalized, err := healthcheck.NormalizePolicy(policy)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return normalized, nil
}

func validateHealthCheckCompatibility(policy *healthcheck.Policy, networkEnabled bool, skipGuestAgent bool) error {
	if !healthcheck.Enabled(policy) {
		return nil
	}
	switch policy.Type {
	case healthcheck.TypeHTTP, healthcheck.TypeTCP:
		if !networkEnabled {
			return fmt.Errorf("%w: %s health checks require network.enabled=true", ErrInvalidRequest, policy.Type)
		}
	case healthcheck.TypeExec:
		if skipGuestAgent {
			return fmt.Errorf("%w: exec health checks require skip_guest_agent=false", ErrInvalidRequest)
		}
	}
	return nil
}

// GetHealthCheckRuntime returns persisted health check runtime status.
func (m *manager) GetHealthCheckRuntime(_ context.Context, id string) (*healthcheck.Runtime, error) {
	meta, err := m.loadMetadata(id)
	if err != nil {
		return nil, err
	}
	return healthcheck.CloneRuntime(meta.HealthCheckRuntime), nil
}

// SetHealthCheckRuntime persists health check runtime status.
func (m *manager) SetHealthCheckRuntime(_ context.Context, id string, runtime *healthcheck.Runtime) error {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()

	meta, err := m.loadMetadata(id)
	if err != nil {
		return err
	}
	meta.HealthCheckRuntime = healthcheck.CloneRuntime(runtime)
	return m.saveMetadata(meta)
}
