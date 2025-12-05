package instances

import (
	"context"
	"fmt"
)

// IngressResolver provides instance resolution for the ingress package.
// It implements ingress.InstanceResolver interface without importing the ingress package
// to avoid import cycles.
type IngressResolver struct {
	manager Manager
}

// NewIngressResolver creates a new IngressResolver that wraps an instance manager.
func NewIngressResolver(manager Manager) *IngressResolver {
	return &IngressResolver{manager: manager}
}

// ResolveInstanceIP resolves an instance name, ID, or ID prefix to its IP address.
func (r *IngressResolver) ResolveInstanceIP(ctx context.Context, nameOrID string) (string, error) {
	inst, err := r.manager.GetInstance(ctx, nameOrID)
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
func (r *IngressResolver) InstanceExists(ctx context.Context, nameOrID string) (bool, error) {
	_, err := r.manager.GetInstance(ctx, nameOrID)
	return err == nil, nil
}
