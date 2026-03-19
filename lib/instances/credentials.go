package instances

import (
	"context"
	"fmt"

	"github.com/kernel/hypeman/lib/logger"
)

// updateInstanceCredentials replaces credential policies for an instance.
// It validates the new credentials, updates stored metadata, and refreshes the
// live egress proxy policy if the instance has one registered.
func (m *manager) updateInstanceCredentials(ctx context.Context, id string, credentials map[string]CredentialPolicy, env map[string]string) (*Instance, error) {
	log := logger.FromContext(ctx)

	meta, err := m.loadMetadata(id)
	if err != nil {
		return nil, ErrNotFound
	}
	stored := &meta.StoredMetadata

	// Credentials require an active egress proxy policy
	if len(credentials) > 0 {
		if stored.NetworkEgress == nil || !stored.NetworkEgress.Enabled {
			return nil, fmt.Errorf("%w: credentials require network.egress.enabled=true", ErrInvalidRequest)
		}
	}

	// Normalize and validate credential policies
	normalized, err := normalizeCredentialPolicies(credentials)
	if err != nil {
		return nil, err
	}

	// Merge supplied env values into the stored env for validation.
	// We build a merged view: stored env overridden by supplied env.
	mergedEnv := make(map[string]string, len(stored.Env)+len(env))
	for k, v := range stored.Env {
		mergedEnv[k] = v
	}
	for k, v := range env {
		mergedEnv[k] = v
	}

	if len(normalized) > 0 {
		if err := validateCredentialEnvBindings(normalized, mergedEnv); err != nil {
			return nil, err
		}
	}

	// Update stored metadata
	stored.Credentials = cloneCredentialPolicies(normalized)
	// Persist new env values referenced by credentials
	for k, v := range env {
		stored.Env[k] = v
	}

	// Update live egress proxy policy if one is registered
	m.egressProxyMu.Lock()
	svc := m.egressProxy
	m.egressProxyMu.Unlock()

	if svc != nil {
		rules := buildEgressProxyInjectRules(stored.NetworkEgress, stored.Credentials, stored.Env)
		updated, err := svc.UpdateInstancePolicy(id, rules)
		if err != nil {
			log.ErrorContext(ctx, "failed to update egress proxy policy", "instance_id", id, "error", err)
			return nil, fmt.Errorf("update egress proxy policy: %w", err)
		}
		if updated {
			log.InfoContext(ctx, "updated egress proxy credential policy", "instance_id", id)
		}
	}

	// Save metadata
	if err := m.saveMetadata(&metadata{StoredMetadata: *stored}); err != nil {
		return nil, fmt.Errorf("save metadata: %w", err)
	}

	log.InfoContext(ctx, "updated instance credentials", "instance_id", id, "credential_count", len(normalized))

	inst := m.toInstanceWithoutHydration(ctx, &metadata{StoredMetadata: *stored})
	return &inst, nil
}
