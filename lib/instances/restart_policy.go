package instances

import (
	"context"
	"fmt"

	"github.com/kernel/hypeman/lib/logger"
	restartpolicy "github.com/kernel/hypeman/lib/restart-policy"
)

type restartPolicyStore struct {
	manager *manager
}

func cloneRestartPolicy(policy *restartpolicy.Policy) *restartpolicy.Policy {
	if policy == nil {
		return nil
	}
	return &restartpolicy.Policy{
		Policy:      policy.Policy,
		Backoff:     policy.Backoff,
		MaxAttempts: policy.MaxAttempts,
		StableAfter: policy.StableAfter,
	}
}

func normalizeRestartPolicy(policy *restartpolicy.Policy) (*restartpolicy.Policy, error) {
	normalized, err := restartpolicy.NormalizePolicy(policy)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return normalized, nil
}

func restartStatusAfterPolicyUpdate(status restartpolicy.Status) restartpolicy.Status {
	if status.BlockedReason == restartpolicy.BlockedReasonManualStop {
		return restartpolicy.Status{BlockedReason: restartpolicy.BlockedReasonManualStop}
	}
	return restartpolicy.Status{}
}

func (m *manager) markRestartManualStopLocked(ctx context.Context, id string) error {
	if err := m.updateRestartStatusLocked(id, restartpolicy.Status{BlockedReason: restartpolicy.BlockedReasonManualStop}); err != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to mark restart policy manual stop", "instance_id", id, "error", err)
		return err
	}
	return nil
}

func (m *manager) clearRestartStatusLocked(ctx context.Context, id string) error {
	if err := m.updateRestartStatusLocked(id, restartpolicy.Status{}); err != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to clear restart policy status", "instance_id", id, "error", err)
		return err
	}
	return nil
}

func (m *manager) updateRestartStatusLocked(id string, status restartpolicy.Status) error {
	meta, err := m.loadMetadata(id)
	if err != nil {
		return err
	}
	meta.RestartStatus = status
	return m.saveMetadata(meta)
}

func (m *manager) RestartInstance(ctx context.Context, id string) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()

	inst, err := m.startInstance(ctx, id, StartInstanceRequest{})
	if err == nil {
		m.notifyLifecycleEvent(ctx, LifecycleEventStart, inst)
	}
	return inst, err
}

func (m *manager) StartRestartPolicyController(ctx context.Context) error {
	controller := restartpolicy.NewController(
		restartPolicyStore{manager: m},
		restartpolicy.ControllerOptions{
			Log: logger.FromContext(ctx).With("controller", "restart_policy"),
		},
	)
	return controller.Run(ctx)
}

func (s restartPolicyStore) ListInstances(ctx context.Context) ([]restartpolicy.Instance, error) {
	insts, err := s.manager.ListInstances(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := make([]restartpolicy.Instance, 0, len(insts))
	for _, inst := range insts {
		out = append(out, *toRestartPolicyInstance(&inst))
	}
	return out, nil
}

func (s restartPolicyStore) RestartInstance(ctx context.Context, id string) error {
	_, err := s.manager.RestartInstance(ctx, id)
	return err
}

func (s restartPolicyStore) SetRestartStatus(ctx context.Context, id string, status restartpolicy.Status) error {
	lock := s.manager.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	return s.manager.updateRestartStatusLocked(id, status)
}

func (s restartPolicyStore) SubscribeInstanceEvents() (<-chan restartpolicy.InstanceEvent, func(), error) {
	src, unsub := s.manager.SubscribeLifecycleEvents(LifecycleEventConsumerRestartPolicy)
	dst := make(chan restartpolicy.InstanceEvent, 32)
	go func() {
		defer close(dst)
		for event := range src {
			dst <- restartpolicy.InstanceEvent{
				Action:     restartpolicy.InstanceEventAction(event.Action),
				InstanceID: event.InstanceID,
				Instance:   toRestartPolicyInstance(event.Instance),
			}
		}
	}()
	return dst, unsub, nil
}

func toRestartPolicyInstance(inst *Instance) *restartpolicy.Instance {
	if inst == nil {
		return nil
	}
	return &restartpolicy.Instance{
		ID:            inst.Id,
		State:         string(inst.State),
		StartedAt:     inst.StartedAt,
		ExitCode:      inst.ExitCode,
		RestartPolicy: inst.RestartPolicy,
		RestartStatus: inst.RestartStatus,
	}
}

var _ restartpolicy.Store = restartPolicyStore{}
var _ interface {
	StartRestartPolicyController(context.Context) error
} = (*manager)(nil)
var _ interface {
	RestartInstance(context.Context, string) (*Instance, error)
} = (*manager)(nil)
