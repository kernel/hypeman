package instances

import (
	"context"
	"fmt"

	"github.com/kernel/hypeman/lib/logger"
)

// updateInstance updates mutable properties of a running instance.
// Currently supports updating env vars referenced by credential policies,
// which causes the egress proxy header inject rules to be recomputed
// with the new secret values — enabling key rotation without restart.
func (m *manager) updateInstance(ctx context.Context, id string, req UpdateInstanceRequest) (*Instance, error) {
	log := logger.FromContext(ctx)

	// 1. Load and validate current state
	meta, err := m.loadMetadata(id)
	if err != nil {
		return nil, ErrNotFound
	}

	inst, err := m.getInstance(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get instance: %w", err)
	}

	if inst.State != StateRunning && inst.State != StateInitializing {
		return nil, fmt.Errorf("%w: instance must be running or initializing to update (current state: %s)", ErrInvalidState, inst.State)
	}

	// 2. Merge new env vars into existing env
	if len(req.Env) > 0 {
		if meta.Env == nil {
			meta.Env = make(map[string]string)
		}
		for k, v := range req.Env {
			meta.Env[k] = v
		}
	}

	// 3. If credentials are configured, validate bindings and update egress proxy rules
	if len(meta.Credentials) > 0 && meta.NetworkEgress != nil && meta.NetworkEgress.Enabled {
		if err := validateCredentialEnvBindings(meta.Credentials, meta.Env); err != nil {
			return nil, err
		}

		newRules := buildEgressProxyInjectRules(meta.NetworkEgress, meta.Credentials, meta.Env)

		svc := m.getEgressProxyIfExists()
		if svc != nil {
			if err := svc.UpdateInstanceRules(id, newRules); err != nil {
				return nil, fmt.Errorf("update egress proxy rules: %w", err)
			}
			log.InfoContext(ctx, "updated egress proxy header inject rules", "instance_id", id)
		}
	}

	// 4. Persist updated metadata
	if err := m.saveMetadata(meta); err != nil {
		return nil, fmt.Errorf("save metadata: %w", err)
	}

	log.InfoContext(ctx, "instance updated", "instance_id", id)

	// 5. Return updated instance
	updated, err := m.getInstance(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get updated instance: %w", err)
	}
	return updated, nil
}
