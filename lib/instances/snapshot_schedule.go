package instances

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/scheduledsnapshots"
	"github.com/kernel/hypeman/lib/tags"
)

func (m *manager) SetSnapshotSchedule(ctx context.Context, instanceID string, req SetSnapshotScheduleRequest) (*SnapshotSchedule, error) {
	if err := validateSetSnapshotScheduleRequest(req); err != nil {
		return nil, err
	}

	lock := m.getInstanceLock(instanceID)
	lock.Lock()
	defer lock.Unlock()

	if _, err := m.loadMetadata(instanceID); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	createdAt := now
	if existing, err := m.getSnapshotScheduleUnlocked(instanceID); err == nil {
		createdAt = existing.CreatedAt
	} else if !errors.Is(err, ErrSnapshotScheduleNotFound) {
		return nil, err
	}

	schedule := &SnapshotSchedule{
		InstanceID: instanceID,
		Interval:   req.Interval,
		NamePrefix: req.NamePrefix,
		Metadata:   tags.Clone(req.Metadata),
		Retention:  req.Retention,
		NextRunAt:  now.Add(req.Interval),
		CreatedAt:  createdAt,
		UpdatedAt:  now,
	}

	if err := m.saveSnapshotScheduleUnlocked(schedule); err != nil {
		return nil, err
	}

	return schedule, nil
}

func (m *manager) GetSnapshotSchedule(ctx context.Context, instanceID string) (*SnapshotSchedule, error) {
	lock := m.getInstanceLock(instanceID)
	lock.RLock()
	defer lock.RUnlock()

	if _, err := m.loadMetadata(instanceID); err != nil {
		return nil, err
	}
	return m.getSnapshotScheduleUnlocked(instanceID)
}

func (m *manager) DeleteSnapshotSchedule(ctx context.Context, instanceID string) error {
	lock := m.getInstanceLock(instanceID)
	lock.Lock()
	defer lock.Unlock()

	if _, err := m.loadMetadata(instanceID); err != nil {
		return err
	}

	err := os.Remove(m.paths.InstanceSnapshotSchedule(instanceID))
	if err == nil {
		return nil
	}
	if os.IsNotExist(err) {
		return ErrSnapshotScheduleNotFound
	}
	return fmt.Errorf("delete snapshot schedule: %w", err)
}

func (m *manager) RunSnapshotSchedules(ctx context.Context) error {
	log := logger.FromContext(ctx)
	instanceIDs, err := m.listSnapshotScheduleInstanceIDs()
	if err != nil {
		return fmt.Errorf("list snapshot schedules: %w", err)
	}

	now := time.Now().UTC()
	var runErrs []error

	for _, instanceID := range instanceIDs {
		lock := m.getInstanceLock(instanceID)
		lock.RLock()
		due, err := m.snapshotScheduleDueLocked(instanceID, now)
		lock.RUnlock()
		if err != nil {
			if errors.Is(err, ErrSnapshotScheduleNotFound) {
				continue
			}
			runErrs = append(runErrs, fmt.Errorf("instance %s: %w", instanceID, err))
			log.ErrorContext(ctx, "scheduled snapshot due-check failed", "instance_id", instanceID, "error", err)
			continue
		}
		if !due {
			continue
		}

		lock.Lock()
		due, err = m.snapshotScheduleDueLocked(instanceID, now)
		if err != nil {
			lock.Unlock()
			if errors.Is(err, ErrSnapshotScheduleNotFound) {
				continue
			}
			runErrs = append(runErrs, fmt.Errorf("instance %s: %w", instanceID, err))
			log.ErrorContext(ctx, "scheduled snapshot due-check failed", "instance_id", instanceID, "error", err)
			continue
		}
		if !due {
			lock.Unlock()
			continue
		}

		err = m.runSnapshotScheduleForInstanceLocked(ctx, instanceID, now)
		lock.Unlock()
		if err != nil {
			runErrs = append(runErrs, fmt.Errorf("instance %s: %w", instanceID, err))
			log.ErrorContext(ctx, "scheduled snapshot run failed", "instance_id", instanceID, "error", err)
		}
	}

	if len(runErrs) > 0 {
		return errors.Join(runErrs...)
	}
	return nil
}

func (m *manager) snapshotScheduleDueLocked(instanceID string, now time.Time) (bool, error) {
	schedule, err := m.getSnapshotScheduleUnlocked(instanceID)
	if err != nil {
		return false, err
	}
	return !now.Before(schedule.NextRunAt), nil
}

func (m *manager) runSnapshotScheduleForInstanceLocked(ctx context.Context, instanceID string, now time.Time) error {
	schedule, err := m.getSnapshotScheduleUnlocked(instanceID)
	if err != nil {
		if errors.Is(err, ErrSnapshotScheduleNotFound) {
			return nil
		}
		return err
	}
	if now.Before(schedule.NextRunAt) {
		return nil
	}

	runTime := now.UTC()
	schedule.NextRunAt = scheduledsnapshots.NextRun(schedule.NextRunAt, schedule.Interval, runTime)
	schedule.LastRunAt = &runTime
	schedule.LastSnapshotID = nil
	schedule.LastError = nil
	schedule.UpdatedAt = runTime

	sourceMeta, sourceErr := m.loadMetadata(instanceID)
	instanceMissing := errors.Is(sourceErr, ErrNotFound)
	if sourceErr != nil && !instanceMissing {
		errMsg := sourceErr.Error()
		schedule.LastError = &errMsg
		if saveErr := m.saveSnapshotScheduleUnlocked(schedule); saveErr != nil {
			return fmt.Errorf("load source metadata: %w; save schedule: %v", sourceErr, saveErr)
		}
		return fmt.Errorf("load source metadata: %w", sourceErr)
	}

	if !instanceMissing {
		sourceState := m.toInstance(ctx, sourceMeta).State
		snapshotKind, kindErr := scheduledSnapshotKindForState(sourceState)
		if kindErr != nil {
			errMsg := kindErr.Error()
			schedule.LastError = &errMsg
			if saveErr := m.saveSnapshotScheduleUnlocked(schedule); saveErr != nil {
				return fmt.Errorf("resolve scheduled snapshot kind: %w; save schedule: %v", kindErr, saveErr)
			}
			return fmt.Errorf("resolve scheduled snapshot kind: %w", kindErr)
		}

		snapshot, runErr := m.createSnapshot(ctx, instanceID, CreateSnapshotRequest{
			Kind:     snapshotKind,
			Name:     scheduledsnapshots.BuildSnapshotName(schedule.NamePrefix, runTime, validateInstanceName),
			Metadata: scheduledsnapshots.BuildSnapshotMetadata(instanceID, schedule.Metadata),
		})
		if runErr != nil {
			errMsg := runErr.Error()
			schedule.LastError = &errMsg
			if saveErr := m.saveSnapshotScheduleUnlocked(schedule); saveErr != nil {
				return fmt.Errorf("create scheduled snapshot: %w; save schedule: %v", runErr, saveErr)
			}
			return fmt.Errorf("create scheduled snapshot: %w", runErr)
		}

		snapshotID := snapshot.Id
		schedule.LastSnapshotID = &snapshotID
	}

	cleanupErr := m.cleanupScheduledSnapshots(ctx, instanceID, schedule.Retention, runTime)
	if cleanupErr != nil {
		errMsg := cleanupErr.Error()
		schedule.LastError = &errMsg
	}

	if instanceMissing && cleanupErr == nil {
		remaining, countErr := m.countScheduledSnapshots(ctx, instanceID)
		if countErr != nil {
			errMsg := countErr.Error()
			schedule.LastError = &errMsg
			cleanupErr = fmt.Errorf("count scheduled snapshots: %w", countErr)
		} else {
			shouldDeleteSchedule := remaining == 0
			if !shouldDeleteSchedule && schedule.Retention.MaxAge == 0 && schedule.Retention.MaxCount > 0 && remaining <= schedule.Retention.MaxCount {
				// Count-only retention has converged for deleted instances: no future run can reduce count.
				shouldDeleteSchedule = true
			}
			if shouldDeleteSchedule {
				if err := os.Remove(m.paths.InstanceSnapshotSchedule(instanceID)); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("delete snapshot schedule after source deletion: %w", err)
				}
				return nil
			}
		}
	}

	if saveErr := m.saveSnapshotScheduleUnlocked(schedule); saveErr != nil {
		if cleanupErr != nil {
			return fmt.Errorf("cleanup scheduled snapshots: %w; save schedule: %v", cleanupErr, saveErr)
		}
		return fmt.Errorf("save schedule: %w", saveErr)
	}
	if cleanupErr != nil {
		return fmt.Errorf("cleanup scheduled snapshots: %w", cleanupErr)
	}

	return nil
}

func (m *manager) cleanupScheduledSnapshots(ctx context.Context, instanceID string, retention SnapshotScheduleRetention, now time.Time) error {
	if retention.MaxCount == 0 && retention.MaxAge == 0 {
		return nil
	}

	filter := &ListSnapshotsFilter{SourceInstanceID: &instanceID}
	snapshots, err := m.listSnapshots(ctx, filter)
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}

	type candidate struct {
		id        string
		createdAt time.Time
	}
	candidates := make([]candidate, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if !scheduledsnapshots.IsScheduledSnapshot(snapshot.Metadata, instanceID) {
			continue
		}
		candidates = append(candidates, candidate{id: snapshot.Id, createdAt: snapshot.CreatedAt})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].createdAt.After(candidates[j].createdAt)
	})

	deleteIDs := make(map[string]struct{})
	if retention.MaxCount > 0 && len(candidates) > retention.MaxCount {
		for _, candidate := range candidates[retention.MaxCount:] {
			deleteIDs[candidate.id] = struct{}{}
		}
	}
	if retention.MaxAge > 0 {
		cutoff := now.Add(-retention.MaxAge)
		for _, candidate := range candidates {
			if candidate.createdAt.Before(cutoff) {
				deleteIDs[candidate.id] = struct{}{}
			}
		}
	}

	if len(deleteIDs) == 0 {
		return nil
	}

	errs := make([]error, 0)
	for snapshotID := range deleteIDs {
		if err := m.deleteSnapshot(ctx, snapshotID); err != nil {
			if errors.Is(err, ErrSnapshotNotFound) {
				continue
			}
			errs = append(errs, fmt.Errorf("delete snapshot %s: %w", snapshotID, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return nil
}

func validateSetSnapshotScheduleRequest(req SetSnapshotScheduleRequest) error {
	if err := scheduledsnapshots.ValidateSetRequest(req, validateInstanceName); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return nil
}

func (m *manager) getSnapshotScheduleUnlocked(instanceID string) (*SnapshotSchedule, error) {
	content, err := os.ReadFile(m.paths.InstanceSnapshotSchedule(instanceID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrSnapshotScheduleNotFound
		}
		return nil, fmt.Errorf("read snapshot schedule: %w", err)
	}

	schedule, err := scheduledsnapshots.UnmarshalSchedule(content)
	if err != nil {
		return nil, err
	}
	return schedule, nil
}

func (m *manager) saveSnapshotScheduleUnlocked(schedule *SnapshotSchedule) error {
	content, err := scheduledsnapshots.MarshalSchedule(schedule)
	if err != nil {
		return fmt.Errorf("marshal snapshot schedule: %w", err)
	}

	if err := os.MkdirAll(m.paths.SnapshotSchedulesDir(), 0755); err != nil {
		return fmt.Errorf("create snapshot schedules directory: %w", err)
	}
	if err := os.WriteFile(m.paths.InstanceSnapshotSchedule(schedule.InstanceID), content, 0644); err != nil {
		return fmt.Errorf("write snapshot schedule: %w", err)
	}

	return nil
}

func (m *manager) listSnapshotScheduleInstanceIDs() ([]string, error) {
	return scheduledsnapshots.ListInstanceIDs(m.paths.SnapshotSchedulesDir())
}

func (m *manager) countScheduledSnapshots(ctx context.Context, instanceID string) (int, error) {
	filter := &ListSnapshotsFilter{SourceInstanceID: &instanceID}
	snapshots, err := m.listSnapshots(ctx, filter)
	if err != nil {
		return 0, fmt.Errorf("list snapshots: %w", err)
	}

	count := 0
	for _, snapshot := range snapshots {
		if scheduledsnapshots.IsScheduledSnapshot(snapshot.Metadata, instanceID) {
			count++
		}
	}
	return count, nil
}

func scheduledSnapshotKindForState(state State) (SnapshotKind, error) {
	switch state {
	case StateStopped:
		return SnapshotKindStopped, nil
	case StateRunning, StateStandby:
		return SnapshotKindStandby, nil
	default:
		return "", fmt.Errorf("%w: scheduled snapshot requires source in %s, %s, or %s, got %s", ErrInvalidState, StateRunning, StateStandby, StateStopped, state)
	}
}
