package instances

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/kernel/hypeman/lib/logger"
	restartpolicy "github.com/kernel/hypeman/lib/restart-policy"
)

const defaultRestartPolicyReconcileInterval = 5 * time.Second

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

func (m *manager) setRestartStatus(ctx context.Context, id string, status restartpolicy.Status) error {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	return m.updateRestartStatusLocked(id, status)
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

func (m *manager) HandleHealthCheckUnhealthy(ctx context.Context, id string) error {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()

	current, err := m.currentInstanceWithoutHydration(ctx, id)
	if err != nil {
		return err
	}
	if current.State != StateRunning {
		return nil
	}

	policy, err := restartpolicy.NormalizePolicy(current.RestartPolicy)
	if err != nil {
		return err
	}
	if !restartpolicy.ShouldRestart(policy, nil) {
		return nil
	}
	if current.RestartStatus.BlockedReason != "" {
		return nil
	}

	now := time.Now().UTC()
	status := current.RestartStatus
	status.LastReason = restartpolicy.RestartReasonHealthCheckFailed
	nextStatus, reason, shouldAttempt := prepareRestartAttempt(policy, status, now)
	if !shouldAttempt {
		if !restartpolicy.EqualStatus(current.RestartStatus, nextStatus) {
			return m.updateRestartStatusLocked(id, nextStatus)
		}
		return nil
	}

	if err := m.updateRestartStatusLocked(id, nextStatus); err != nil {
		return err
	}

	stopped, err := m.stopInstance(ctx, id)
	if err != nil {
		_ = m.updateRestartStatusLocked(id, restartStatusAfterFailedAttempt(policy, nextStatus, reason, now))
		return err
	}
	m.notifyLifecycleEvent(ctx, LifecycleEventStop, stopped)

	started, err := m.startInstance(ctx, id, StartInstanceRequest{})
	if err != nil {
		_ = m.updateRestartStatusLocked(id, restartStatusAfterFailedAttempt(policy, nextStatus, reason, now))
		return err
	}
	m.notifyLifecycleEvent(ctx, LifecycleEventStart, started)
	return nil
}

func (m *manager) StartRestartPolicyController(ctx context.Context) error {
	log := logger.FromContext(ctx).With("controller", "restart_policy")
	log.InfoContext(ctx, "restart policy controller started", "reconcile_interval", defaultRestartPolicyReconcileInterval)
	if err := m.reconcileRestartPolicies(ctx, log); err != nil {
		log.WarnContext(ctx, "restart policy startup reconcile failed", "error", err)
	}

	events, unsubscribe := m.SubscribeLifecycleEvents(LifecycleEventConsumerRestartPolicy)
	defer unsubscribe()

	ticker := time.NewTicker(defaultRestartPolicyReconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-events:
			if !ok {
				return nil
			}
			if event.Action == LifecycleEventDelete {
				continue
			}
			if event.Instance == nil {
				if err := m.reconcileRestartPolicies(ctx, log); err != nil {
					log.WarnContext(ctx, "restart policy event reconcile failed", "instance_id", event.InstanceID, "error", err)
				}
				continue
			}
			if err := m.reconcileRestartPolicyInstance(ctx, event.Instance, log); err != nil {
				log.WarnContext(ctx, "restart policy event handling failed", "instance_id", event.InstanceID, "error", err)
			}
		case <-ticker.C:
			if err := m.reconcileRestartPolicies(ctx, log); err != nil {
				log.WarnContext(ctx, "restart policy reconcile failed", "error", err)
			}
		}
	}
}

func (m *manager) reconcileRestartPolicies(ctx context.Context, log *slog.Logger) error {
	insts, err := m.ListInstances(ctx, nil)
	if err != nil {
		return err
	}
	for i := range insts {
		if err := m.reconcileRestartPolicyInstance(ctx, &insts[i], log); err != nil {
			log.WarnContext(ctx, "restart policy reconcile failed for instance", "instance_id", insts[i].Id, "error", err)
		}
	}
	return nil
}

func (m *manager) reconcileRestartPolicyInstance(ctx context.Context, inst *Instance, log *slog.Logger) error {
	if inst == nil {
		return nil
	}
	policy, err := restartpolicy.NormalizePolicy(inst.RestartPolicy)
	if err != nil {
		return err
	}
	if policy == nil {
		return nil
	}

	status := inst.RestartStatus
	now := time.Now().UTC()
	if shouldResetRestartAttempts(policy, status, inst, now) {
		log.InfoContext(ctx, "restart policy stable window reached", "instance_id", inst.Id, "attempts", status.Attempts)
		return m.setRestartStatus(ctx, inst.Id, restartpolicy.Status{})
	}
	if inst.State != StateStopped || status.BlockedReason != "" {
		return nil
	}

	exitCode := inst.ExitCode
	if status.LastReason == restartpolicy.RestartReasonHealthCheckFailed {
		exitCode = nil
	}
	if !restartpolicy.ShouldRestart(policy, exitCode) {
		return nil
	}
	return m.startInstanceForRestartPolicy(ctx, inst.Id, policy, status, now, log)
}

func (m *manager) startInstanceForRestartPolicy(ctx context.Context, id string, policy *restartpolicy.Policy, status restartpolicy.Status, now time.Time, log *slog.Logger) error {
	nextStatus, reason, shouldAttempt := prepareRestartAttempt(policy, status, now)
	if !shouldAttempt {
		if !restartpolicy.EqualStatus(status, nextStatus) {
			return m.setRestartStatus(ctx, id, nextStatus)
		}
		return nil
	}
	if err := m.setRestartStatus(ctx, id, nextStatus); err != nil {
		return err
	}

	log.InfoContext(ctx, "restart policy starting instance", "instance_id", id, "attempt", nextStatus.Attempts)
	if _, err := m.RestartInstance(ctx, id); err != nil {
		nextStatus = restartStatusAfterFailedAttempt(policy, nextStatus, reason, now)
		if statusErr := m.setRestartStatus(ctx, id, nextStatus); statusErr != nil {
			log.WarnContext(ctx, "failed to persist restart status after restart failure", "instance_id", id, "error", statusErr)
		}
		return err
	}
	return nil
}

func prepareRestartAttempt(policy *restartpolicy.Policy, status restartpolicy.Status, now time.Time) (restartpolicy.Status, restartpolicy.RestartReason, bool) {
	nextStatus, shouldAttempt := restartpolicy.PrepareAttempt(policy, status, now)
	if !shouldAttempt {
		return nextStatus, "", false
	}
	reason := nextStatus.LastReason
	nextStatus.LastReason = ""
	return nextStatus, reason, true
}

func restartStatusAfterFailedAttempt(policy *restartpolicy.Policy, status restartpolicy.Status, reason restartpolicy.RestartReason, now time.Time) restartpolicy.Status {
	status = restartpolicy.AfterFailedAttempt(policy, status, now)
	status.LastReason = reason
	return status
}

func shouldResetRestartAttempts(policy *restartpolicy.Policy, status restartpolicy.Status, inst *Instance, now time.Time) bool {
	if status.Attempts == 0 || inst.StartedAt == nil {
		return false
	}
	if inst.State != StateRunning && inst.State != StateInitializing {
		return false
	}
	return !now.Before(inst.StartedAt.UTC().Add(restartpolicy.StableAfter(policy)))
}
