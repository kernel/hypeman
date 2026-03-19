package instances

import (
	"context"
	"fmt"

	"github.com/kernel/hypeman/lib/egressproxy"
)

// UpdateInstanceEnv updates environment variables on a running instance and
// re-registers egress proxy inject rules so that rotated credentials take
// effect without restarting the VM.
func (m *manager) UpdateInstanceEnv(ctx context.Context, id string, env map[string]string) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()

	return m.updateInstanceEnv(ctx, id, env)
}

func (m *manager) updateInstanceEnv(ctx context.Context, id string, env map[string]string) (*Instance, error) {
	meta, err := m.loadMetadata(id)
	if err != nil {
		return nil, ErrNotFound
	}

	inst := m.toInstance(ctx, meta)
	if inst.State != StateRunning {
		return nil, fmt.Errorf("%w: instance must be running to update env", ErrInvalidState)
	}

	if meta.NetworkEgress == nil || !meta.NetworkEgress.Enabled {
		return nil, fmt.Errorf("%w: instance does not have egress proxy enabled", ErrInvalidRequest)
	}

	// Merge new env values into stored env
	if meta.Env == nil {
		meta.Env = make(map[string]string, len(env))
	}
	for k, v := range env {
		meta.Env[k] = v
	}

	// Re-validate credential bindings against updated env
	if err := validateCredentialEnvBindings(meta.Credentials, meta.Env); err != nil {
		return nil, err
	}

	// Persist updated metadata
	if err := m.saveMetadata(meta); err != nil {
		return nil, fmt.Errorf("save metadata: %w", err)
	}

	// Rebuild inject rules with new env values
	rules := buildEgressProxyInjectRules(meta.NetworkEgress, meta.Credentials, meta.Env)

	// Re-register with proxy service using updated rules
	svc, err := m.getOrCreateEgressProxyService()
	if err != nil {
		return nil, fmt.Errorf("get egress proxy service: %w", err)
	}

	alloc, err := m.networkManager.GetAllocation(ctx, meta.Id)
	if err != nil {
		return nil, fmt.Errorf("get network allocation: %w", err)
	}

	_, err = svc.RegisterInstance(ctx, alloc.Gateway, egressproxy.InstanceConfig{
		InstanceID:        meta.Id,
		SourceIP:          alloc.IP,
		TAPDevice:         alloc.TAPDevice,
		BlockAllTCPEgress: meta.NetworkEgress.EnforcementMode != EgressEnforcementModeHTTPHTTPSOnly,
		HeaderInjectRules: rules,
	})
	if err != nil {
		return nil, fmt.Errorf("re-register egress proxy: %w", err)
	}

	// Return updated instance
	updated := m.toInstance(ctx, meta)
	return &updated, nil
}
