package instances

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
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
	meta, err := m.loadMetadata(id)
	if err != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to mark restart policy manual stop", "instance_id", id, "error", err)
		return err
	}
	if meta.RestartPolicy == nil {
		return nil
	}

	status := restartpolicy.Status{BlockedReason: restartpolicy.BlockedReasonManualStop}
	if restartpolicy.EqualStatus(meta.RestartStatus, status) {
		return nil
	}
	meta.RestartStatus = status
	if err := m.saveMetadata(meta); err != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to mark restart policy manual stop", "instance_id", id, "error", err)
		return err
	}
	return nil
}

func (m *manager) clearRestartStatusLocked(ctx context.Context, id string) error {
	meta, err := m.loadMetadata(id)
	if err != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to clear restart policy status", "instance_id", id, "error", err)
		return err
	}
	if meta.RestartStatus.IsZero() {
		return nil
	}
	meta.RestartStatus = restartpolicy.Status{}
	if err := m.saveMetadata(meta); err != nil {
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

func (m *manager) setRestartStatus(ctx context.Context, id string, status restartpolicy.Status) (bool, error) {
	return m.setRestartStatusIf(ctx, id, status, nil)
}

func (m *manager) setRestartStatusIfStopped(ctx context.Context, id string, status restartpolicy.Status) (bool, error) {
	return m.setRestartStatusIf(ctx, id, status, func(meta *metadata) bool {
		return m.toInstanceWithoutHydration(ctx, meta).State == StateStopped
	})
}

func (m *manager) setRestartStatusIf(ctx context.Context, id string, status restartpolicy.Status, shouldWrite func(*metadata) bool) (bool, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()

	meta, err := m.loadMetadata(id)
	if err != nil {
		return false, err
	}
	if meta.RestartStatus.BlockedReason == restartpolicy.BlockedReasonManualStop &&
		status.BlockedReason != restartpolicy.BlockedReasonManualStop {
		return false, nil
	}
	if restartpolicy.EqualStatus(meta.RestartStatus, status) {
		return false, nil
	}
	if shouldWrite != nil && !shouldWrite(meta) {
		return false, nil
	}
	meta.RestartStatus = status
	return true, m.saveMetadata(meta)
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

	stopped, err := m.stopInstance(ctx, id, nil)
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
			if event.InstanceID == "" {
				if err := m.reconcileRestartPolicies(ctx, log); err != nil {
					log.WarnContext(ctx, "restart policy event reconcile failed", "instance_id", event.InstanceID, "error", err)
				}
				continue
			}
			if err := m.reconcileRestartPolicyInstanceID(ctx, event.InstanceID, log); err != nil {
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
	files, err := m.listMetadataFiles()
	if err != nil {
		return err
	}

	for _, file := range files {
		id := filepath.Base(filepath.Dir(file))
		meta, err := m.loadMetadata(id)
		if err != nil {
			log.WarnContext(ctx, "skipping restart policy reconcile for invalid instance metadata", "instance_id", id, "error", err)
			continue
		}
		if meta.RestartPolicy == nil {
			continue
		}
		inst := m.toInstanceWithoutHydration(ctx, meta)
		if err := m.reconcileRestartPolicyInstance(ctx, &inst, log); err != nil {
			log.WarnContext(ctx, "restart policy reconcile failed for instance", "instance_id", inst.Id, "error", err)
		}
	}
	return nil
}

func (m *manager) reconcileRestartPolicyInstanceID(ctx context.Context, id string, log *slog.Logger) error {
	inst, err := m.currentInstanceWithoutHydration(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return m.reconcileRestartPolicyInstance(ctx, inst, log)
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
		_, err := m.setRestartStatus(ctx, inst.Id, restartpolicy.Status{})
		return err
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
			_, err := m.setRestartStatusIfStopped(ctx, id, nextStatus)
			return err
		}
		return nil
	}
	wrote, err := m.setRestartStatusIfStopped(ctx, id, nextStatus)
	if err != nil {
		return err
	}
	if !wrote {
		return nil
	}

	log.InfoContext(ctx, "restart policy starting instance", "instance_id", id, "attempt", nextStatus.Attempts)
	if _, err := m.RestartInstance(ctx, id); err != nil {
		nextStatus = restartStatusAfterFailedAttempt(policy, nextStatus, reason, now)
		if _, statusErr := m.setRestartStatusIfStopped(ctx, id, nextStatus); statusErr != nil {
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
	stableSince := inst.StartedAt.UTC()
	if status.LastAttemptAt != nil && status.LastAttemptAt.UTC().After(stableSince) {
		stableSince = status.LastAttemptAt.UTC()
	}
	return !now.Before(stableSince.Add(restartpolicy.StableAfter(policy)))
}
