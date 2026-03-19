package instances

import (
	"context"
	"fmt"
	"strings"

	"github.com/kernel/hypeman/lib/logger"
)

// updateEgressSecrets performs the actual update of egress proxy secret env values.
// Caller must hold the instance write lock.
func (m *manager) updateEgressSecrets(ctx context.Context, id string, req UpdateEgressSecretsRequest) (*Instance, error) {
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "updating egress proxy secrets", "instance_id", id)

	// 1. Load and validate instance state
	meta, err := m.loadMetadata(id)
	if err != nil {
		return nil, err
	}
	inst := m.toInstance(ctx, meta)
	stored := &meta.StoredMetadata

	if inst.State != StateRunning && inst.State != StateInitializing {
		return nil, fmt.Errorf("%w: instance must be running (current state: %s)", ErrInvalidState, inst.State)
	}

	// 2. Validate egress proxy is enabled
	if stored.NetworkEgress == nil || !stored.NetworkEgress.Enabled {
		return nil, fmt.Errorf("%w: egress proxy is not enabled on this instance", ErrInvalidState)
	}
	if len(stored.Credentials) == 0 {
		return nil, fmt.Errorf("%w: no credential policies configured on this instance", ErrInvalidRequest)
	}

	// 3. Validate the request env vars
	if len(req.Env) == 0 {
		return nil, fmt.Errorf("%w: env must contain at least one entry", ErrInvalidRequest)
	}

	// Build set of env var names referenced by credential policies
	referencedEnvVars := make(map[string]bool)
	for _, policy := range stored.Credentials {
		referencedEnvVars[policy.Source.Env] = true
	}

	for envName, envValue := range req.Env {
		if !referencedEnvVars[envName] {
			return nil, fmt.Errorf("%w: env var %q is not referenced by any credential policy", ErrInvalidRequest, envName)
		}
		if strings.TrimSpace(envValue) == "" {
			return nil, fmt.Errorf("%w: env var %q must be non-empty", ErrInvalidRequest, envName)
		}
	}

	// 4. Update the stored env values
	for envName, envValue := range req.Env {
		stored.Env[envName] = envValue
	}

	// 5. Rebuild and update the proxy inject rules
	newRules := buildEgressProxyInjectRules(stored.NetworkEgress, stored.Credentials, stored.Env)

	m.egressProxyMu.Lock()
	svc := m.egressProxy
	m.egressProxyMu.Unlock()

	if svc == nil {
		return nil, fmt.Errorf("egress proxy service is not running")
	}

	if err := svc.UpdateHeaderInjectRules(id, newRules); err != nil {
		return nil, fmt.Errorf("update egress proxy rules: %w", err)
	}

	// 6. Persist updated metadata
	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		log.ErrorContext(ctx, "failed to save metadata after egress secrets update", "instance_id", id, "error", err)
		return nil, fmt.Errorf("save metadata: %w", err)
	}

	log.InfoContext(ctx, "egress proxy secrets updated", "instance_id", id, "updated_vars", len(req.Env))

	// 7. Return current instance state
	finalInst := m.toInstanceWithoutHydration(ctx, meta)
	return &finalInst, nil
}
