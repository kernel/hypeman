package instances

import (
	"context"
	"fmt"

	"github.com/kernel/hypeman/lib/network"
)

// UpdateInstanceEnv updates environment variables on a running instance and
// re-registers egress proxy header-injection rules with the new values.
// This enables credential rotation without restarting the VM.
func (m *manager) UpdateInstanceEnv(ctx context.Context, id string, req UpdateInstanceEnvRequest) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	return m.updateInstanceEnv(ctx, id, req)
}

func (m *manager) updateInstanceEnv(ctx context.Context, id string, req UpdateInstanceEnvRequest) (*Instance, error) {
	inst, err := m.getInstance(ctx, id)
	if err != nil {
		return nil, ErrNotFound
	}

	if inst.State != StateRunning && inst.State != StateInitializing {
		return nil, fmt.Errorf("%w: instance must be running (current state: %s)", ErrInvalidState, inst.State)
	}

	if inst.NetworkEgress == nil || !inst.NetworkEgress.Enabled {
		return nil, fmt.Errorf("%w: instance does not have egress proxy enabled", ErrInvalidRequest)
	}

	if len(inst.Credentials) == 0 {
		return nil, fmt.Errorf("%w: instance has no credential policies configured", ErrInvalidRequest)
	}

	// Load persisted metadata so we can update and save it
	meta, err := m.loadMetadata(id)
	if err != nil {
		return nil, fmt.Errorf("load metadata: %w", err)
	}

	if meta.Env == nil {
		meta.Env = make(map[string]string)
	}
	for k, v := range req.Env {
		meta.Env[k] = v
	}

	// Validate that all credential bindings are still satisfied
	if err := validateCredentialEnvBindings(meta.Credentials, meta.Env); err != nil {
		return nil, err
	}

	// Get the network allocation for this running instance so we can
	// re-register the proxy with the correct source IP / TAP / gateway.
	alloc, err := m.networkManager.GetAllocation(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get network allocation: %w", err)
	}
	if alloc == nil {
		return nil, fmt.Errorf("no network allocation found for running instance")
	}
	netConfig := &network.NetworkConfig{
		IP:        alloc.IP,
		MAC:       alloc.MAC,
		Gateway:   alloc.Gateway,
		Netmask:   alloc.Netmask,
		DNS:       alloc.DNS,
		TAPDevice: alloc.TAPDevice,
	}

	// Re-register egress proxy with updated inject rules (atomically swaps old rules).
	if _, err := m.maybeRegisterEgressProxy(ctx, &meta.StoredMetadata, netConfig); err != nil {
		return nil, fmt.Errorf("re-register egress proxy: %w", err)
	}

	// Persist updated env to metadata.json
	if err := m.saveMetadata(meta); err != nil {
		return nil, fmt.Errorf("save metadata: %w", err)
	}

	// Return fresh instance state
	return m.getInstance(ctx, id)
}
