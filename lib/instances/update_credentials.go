package instances

import (
	"context"
	"fmt"

	"github.com/kernel/hypeman/lib/egressproxy"
	"github.com/kernel/hypeman/lib/logger"
)

// updateCredentials replaces the credential policies for an instance.
// If the instance has an active egress proxy registration (i.e., it is running),
// the proxy policy is updated atomically. For stopped/standby instances, only
// the stored metadata is updated — the proxy policy will be applied on next start/restore.
func (m *manager) updateCredentials(ctx context.Context, id string, req UpdateCredentialsRequest) (*Instance, error) {
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "updating credentials", "instance_id", id)

	// 1. Load instance metadata
	meta, err := m.loadMetadata(id)
	if err != nil {
		return nil, err
	}
	stored := &meta.StoredMetadata

	// 2. Validate: credentials require egress proxy enabled
	if len(req.Credentials) > 0 {
		if stored.NetworkEgress == nil || !stored.NetworkEgress.Enabled {
			return nil, fmt.Errorf("%w: credentials require network.egress.enabled=true", ErrInvalidRequest)
		}
	}

	// 3. Normalize and validate credential policies
	normalized, err := normalizeCredentialPolicies(req.Credentials)
	if err != nil {
		return nil, err
	}

	// 4. Validate credential env bindings against the provided env map
	// Merge stored env with request env (request values override stored values)
	mergedEnv := make(map[string]string, len(stored.Env)+len(req.Env))
	for k, v := range stored.Env {
		mergedEnv[k] = v
	}
	for k, v := range req.Env {
		mergedEnv[k] = v
	}

	if len(normalized) > 0 {
		if err := validateCredentialEnvBindings(normalized, mergedEnv); err != nil {
			return nil, err
		}
	}

	// 5. Update stored metadata
	stored.Credentials = cloneCredentialPolicies(normalized)
	stored.Env = mergedEnv

	// 6. Update proxy policy if instance has an active egress proxy registration
	m.egressProxyMu.Lock()
	svc := m.egressProxy
	m.egressProxyMu.Unlock()

	if svc != nil {
		rules := buildEgressProxyInjectRules(stored.NetworkEgress, stored.Credentials, stored.Env)
		if err := svc.UpdateInstancePolicy(id, rules); err != nil {
			// ErrInstanceNotRegistered means instance isn't running with egress proxy —
			// that's fine, the updated metadata will be picked up on next start/restore.
			if err != egressproxy.ErrInstanceNotRegistered {
				return nil, fmt.Errorf("update egress proxy policy: %w", err)
			}
			log.DebugContext(ctx, "instance not registered with egress proxy, skipping policy update", "instance_id", id)
		} else {
			log.InfoContext(ctx, "updated egress proxy policy", "instance_id", id)
		}
	}

	// 7. Persist metadata
	if err := m.saveMetadata(meta); err != nil {
		return nil, fmt.Errorf("save metadata: %w", err)
	}

	// 8. Return updated instance
	inst := m.toInstance(ctx, meta)
	log.InfoContext(ctx, "credentials updated", "instance_id", id, "credential_count", len(stored.Credentials))
	return &inst, nil
}
