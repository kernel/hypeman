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

type SnapshotScheduleRetention = scheduledsnapshots.Retention
type SnapshotSchedule = scheduledsnapshots.Schedule
type SetSnapshotScheduleRequest = scheduledsnapshots.SetRequest

// SnapshotScheduleManager provides schedule operations in addition to core instance APIs.
type SnapshotScheduleManager interface {
	SetSnapshotSchedule(ctx context.Context, instanceID string, req SetSnapshotScheduleRequest) (*SnapshotSchedule, error)
	GetSnapshotSchedule(ctx context.Context, instanceID string) (*SnapshotSchedule, error)
	DeleteSnapshotSchedule(ctx context.Context, instanceID string) error
	RunSnapshotSchedules(ctx context.Context) error
}

var _ SnapshotScheduleManager = (*manager)(nil)

func (m *manager) SetSnapshotSchedule(ctx context.Context, instanceID string, req SetSnapshotScheduleRequest) (*SnapshotSchedule, error) {
	if err := scheduledsnapshots.ValidateSetRequest(req, validateInstanceName); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	lock := m.getInstanceLock(instanceID)
	lock.Lock()
	defer lock.Unlock()

	if _, err := m.loadMetadata(instanceID); err != nil {
		return nil, err
	}

	now := m.now().UTC()
	createdAt := now
	nextRunAt := scheduledsnapshots.InitialNextRunAt(instanceID, req.Interval, now)
	var lastRunAt *time.Time
	var lastSnapshotID *string
	var lastError *string
	if existing, err := m.getSnapshotScheduleUnlocked(instanceID); err == nil {
		createdAt = existing.CreatedAt
		if existing.Interval == req.Interval && !existing.NextRunAt.IsZero() {
			nextRunAt = existing.NextRunAt
		}
		if existing.LastRunAt != nil {
			lastRunAtValue := *existing.LastRunAt
			lastRunAt = &lastRunAtValue
		}
		if existing.LastSnapshotID != nil {
			lastSnapshotIDValue := *existing.LastSnapshotID
			lastSnapshotID = &lastSnapshotIDValue
		}
		if existing.LastError != nil {
			lastErrorValue := *existing.LastError
			lastError = &lastErrorValue
		}
	} else if !errors.Is(err, ErrSnapshotScheduleNotFound) {
		return nil, err
	}

	schedule := &SnapshotSchedule{
		InstanceID:     instanceID,
		Interval:       req.Interval,
		NamePrefix:     req.NamePrefix,
		Metadata:       tags.Clone(req.Metadata),
		Retention:      req.Retention,
		NextRunAt:      nextRunAt,
		LastRunAt:      lastRunAt,
		LastSnapshotID: lastSnapshotID,
		LastError:      lastError,
		CreatedAt:      createdAt,
		UpdatedAt:      now,
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

	var runErrs []error

	for _, instanceID := range instanceIDs {
		if err := ctx.Err(); err != nil {
			runErrs = append(runErrs, fmt.Errorf("run snapshot schedules: %w", err))
			break
		}

		readNow := m.now().UTC()
		lock := m.getInstanceLock(instanceID)
		lock.RLock()
		due, err := m.snapshotScheduleDueLocked(instanceID, readNow)
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
		runNow := m.now().UTC()
		due, err = m.snapshotScheduleDueLocked(instanceID, runNow)
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

		err = m.runSnapshotScheduleForInstanceLocked(ctx, instanceID, runNow)
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
		return m.failScheduleRun(schedule, sourceErr, "load source metadata")
	}

	if err := m.saveSnapshotScheduleUnlocked(schedule); err != nil {
		return fmt.Errorf("save schedule before snapshot: %w", err)
	}

	if !instanceMissing {
		sourceState := m.toInstance(ctx, sourceMeta).State
		snapshotKind, kindErr := scheduledSnapshotKindForState(sourceState)
		if kindErr != nil {
			return m.failScheduleRun(schedule, kindErr, "resolve scheduled snapshot kind")
		}

		snapshot, runErr := m.createSnapshot(ctx, instanceID, CreateSnapshotRequest{
			Kind: snapshotKind,
			Name: scheduledsnapshots.BuildSnapshotName(schedule.NamePrefix, runTime),
			Tags: scheduledsnapshots.BuildSnapshotMetadata(instanceID, schedule.Metadata),
		})
		if runErr != nil {
			return m.failScheduleRun(schedule, runErr, "create scheduled snapshot")
		}

		snapshotID := snapshot.Id
		schedule.LastSnapshotID = &snapshotID
		if err := m.saveSnapshotScheduleUnlocked(schedule); err != nil {
			return fmt.Errorf("save schedule after snapshot: %w", err)
		}
	}

	cleanupErr := m.cleanupScheduledSnapshots(ctx, instanceID, schedule.Retention, runTime)
	if cleanupErr != nil {
		errMsg := cleanupErr.Error()
		schedule.LastError = &errMsg
		if saveErr := m.saveSnapshotScheduleUnlocked(schedule); saveErr != nil {
			return fmt.Errorf("cleanup scheduled snapshots: %w; save schedule: %v", cleanupErr, saveErr)
		}
		return fmt.Errorf("cleanup scheduled snapshots: %w", cleanupErr)
	}

	if instanceMissing {
		scheduled, listErr := m.listScheduledSnapshotsByInstance(ctx, instanceID)
		if listErr != nil {
			errMsg := listErr.Error()
			schedule.LastError = &errMsg
			if saveErr := m.saveSnapshotScheduleUnlocked(schedule); saveErr != nil {
				return fmt.Errorf("count scheduled snapshots: %w; save schedule: %v", listErr, saveErr)
			}
			return fmt.Errorf("count scheduled snapshots: %w", listErr)
		} else {
			remaining := len(scheduled)
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

	return nil
}

func (m *manager) cleanupScheduledSnapshots(ctx context.Context, instanceID string, retention SnapshotScheduleRetention, now time.Time) error {
	if retention.MaxCount == 0 && retention.MaxAge == 0 {
		return nil
	}

	scheduledSnapshots, err := m.listScheduledSnapshotsByInstance(ctx, instanceID)
	if err != nil {
		return err
	}

	type candidate struct {
		id        string
		createdAt time.Time
	}
	candidates := make([]candidate, 0, len(scheduledSnapshots))
	for _, snapshot := range scheduledSnapshots {
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

	var errs []error
	for snapshotID := range deleteIDs {
		if err := m.deleteSnapshotFn(ctx, snapshotID); err != nil {
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

func (m *manager) failScheduleRun(schedule *SnapshotSchedule, err error, context string) error {
	errMsg := err.Error()
	schedule.LastError = &errMsg
	if saveErr := m.saveSnapshotScheduleUnlocked(schedule); saveErr != nil {
		return fmt.Errorf("%s: %w; save schedule: %v", context, err, saveErr)
	}
	return fmt.Errorf("%s: %w", context, err)
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
	writeFile := m.writeFile
	if writeFile == nil {
		writeFile = os.WriteFile
	}
	if err := writeFile(m.paths.InstanceSnapshotSchedule(schedule.InstanceID), content, 0644); err != nil {
		return fmt.Errorf("write snapshot schedule: %w", err)
	}

	return nil
}

func (m *manager) listSnapshotScheduleInstanceIDs() ([]string, error) {
	return scheduledsnapshots.ListInstanceIDs(m.paths.SnapshotSchedulesDir())
}

func (m *manager) listScheduledSnapshotsByInstance(ctx context.Context, instanceID string) ([]Snapshot, error) {
	filter := &ListSnapshotsFilter{SourceInstanceID: &instanceID}
	snapshots, err := m.listSnapshots(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}

	scheduledSnapshots := make([]Snapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if scheduledsnapshots.IsScheduledSnapshot(snapshot.Tags, instanceID) {
			scheduledSnapshots = append(scheduledSnapshots, snapshot)
		}
	}
	return scheduledSnapshots, nil
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
