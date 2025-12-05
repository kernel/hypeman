package ingress

import (
	"context"
	"fmt"

	"github.com/onkernel/hypeman/lib/instances"
)

// InstanceResolverAdapter adapts the instance manager to the InstanceResolver interface.
type InstanceResolverAdapter struct {
	instanceManager instances.Manager
}

// NewInstanceResolverAdapter creates a new InstanceResolverAdapter.
func NewInstanceResolverAdapter(instanceManager instances.Manager) *InstanceResolverAdapter {
	return &InstanceResolverAdapter{instanceManager: instanceManager}
}

// ResolveInstanceIP resolves an instance name, ID, or ID prefix to its IP address.
func (a *InstanceResolverAdapter) ResolveInstanceIP(ctx context.Context, nameOrID string) (string, error) {
	inst, err := a.instanceManager.GetInstance(ctx, nameOrID)
	if err != nil {
		return "", fmt.Errorf("instance not found: %s", nameOrID)
	}

	// Check if instance has network enabled
	if !inst.NetworkEnabled {
		return "", fmt.Errorf("instance %s has no network configured", nameOrID)
	}

	// Check if instance has an IP assigned
	if inst.IP == "" {
		return "", fmt.Errorf("instance %s has no IP assigned", nameOrID)
	}

	return inst.IP, nil
}

// InstanceExists checks if an instance with the given name, ID, or ID prefix exists.
func (a *InstanceResolverAdapter) InstanceExists(ctx context.Context, nameOrID string) (bool, error) {
	_, err := a.instanceManager.GetInstance(ctx, nameOrID)
	return err == nil, nil
}
