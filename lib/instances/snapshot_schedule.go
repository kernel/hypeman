package instances

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/tags"
)

const (
	snapshotScheduleMetadataKey        = "hypeman.scheduled"
	snapshotScheduleMetadataInstanceID = "hypeman.schedule_instance_id"
	snapshotScheduleDefaultNamePrefix  = "scheduled"
	minSnapshotScheduleInterval        = time.Minute
)

type snapshotScheduleStorage struct {
	InstanceID     string                        `json:"instance_id"`
	Kind           SnapshotKind                  `json:"kind"`
	Interval       string                        `json:"interval"`
	NamePrefix     string                        `json:"name_prefix,omitempty"`
	Metadata       tags.Metadata                 `json:"metadata,omitempty"`
	Retention      snapshotScheduleRetentionJSON `json:"retention"`
	NextRunAt      time.Time                     `json:"next_run_at"`
	LastRunAt      *time.Time                    `json:"last_run_at,omitempty"`
	LastSnapshotID *string                       `json:"last_snapshot_id,omitempty"`
	LastError      *string                       `json:"last_error,omitempty"`
	CreatedAt      time.Time                     `json:"created_at"`
	UpdatedAt      time.Time                     `json:"updated_at"`
}

type snapshotScheduleRetentionJSON struct {
	MaxCount int    `json:"max_count,omitempty"`
	MaxAge   string `json:"max_age,omitempty"`
}

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
		Kind:       req.Kind,
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
	instances, err := m.listInstances(ctx)
	if err != nil {
		return fmt.Errorf("list instances for scheduled snapshots: %w", err)
	}

	now := time.Now().UTC()
	var runErr error

	for _, inst := range instances {
		lock := m.getInstanceLock(inst.Id)
		lock.Lock()
		err := m.runSnapshotScheduleForInstanceLocked(ctx, inst.Id, now)
		lock.Unlock()
		if err != nil {
			runErr = err
			log.ErrorContext(ctx, "scheduled snapshot run failed", "instance_id", inst.Id, "error", err)
		}
	}

	return runErr
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
	schedule.NextRunAt = nextSnapshotScheduleRun(schedule.NextRunAt, schedule.Interval, runTime)
	schedule.LastRunAt = &runTime
	schedule.LastSnapshotID = nil
	schedule.LastError = nil
	schedule.UpdatedAt = runTime

	snapshot, runErr := m.createSnapshot(ctx, instanceID, CreateSnapshotRequest{
		Kind:     schedule.Kind,
		Name:     buildScheduledSnapshotName(schedule.NamePrefix, runTime),
		Metadata: buildScheduledSnapshotMetadata(instanceID, schedule.Metadata),
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

	cleanupErr := m.cleanupScheduledSnapshots(ctx, instanceID, schedule.Retention, runTime)
	if cleanupErr != nil {
		errMsg := cleanupErr.Error()
		schedule.LastError = &errMsg
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
		if !isScheduledSnapshot(snapshot, instanceID) {
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
			errs = append(errs, fmt.Errorf("delete snapshot %s: %w", snapshotID, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return nil
}

func isScheduledSnapshot(snapshot Snapshot, instanceID string) bool {
	if snapshot.Metadata == nil {
		return false
	}
	if snapshot.Metadata[snapshotScheduleMetadataKey] != "true" {
		return false
	}
	return snapshot.Metadata[snapshotScheduleMetadataInstanceID] == instanceID
}

func buildScheduledSnapshotMetadata(instanceID string, userMetadata tags.Metadata) tags.Metadata {
	metadata := tags.Clone(userMetadata)
	if metadata == nil {
		metadata = make(tags.Metadata)
	}
	metadata[snapshotScheduleMetadataKey] = "true"
	metadata[snapshotScheduleMetadataInstanceID] = instanceID
	return metadata
}

func buildScheduledSnapshotName(prefix string, runAt time.Time) string {
	if prefix == "" {
		prefix = snapshotScheduleDefaultNamePrefix
	}

	suffix := runAt.UTC().Format("20060102-150405")
	maxPrefixLen := 63 - len(suffix) - 1
	if maxPrefixLen < 1 {
		maxPrefixLen = 1
	}

	if len(prefix) > maxPrefixLen {
		prefix = strings.Trim(prefix[:maxPrefixLen], "-")
		if prefix == "" {
			prefix = "s"
		}
	}

	name := prefix + "-" + suffix
	if err := validateInstanceName(name); err != nil {
		return "s-" + suffix
	}
	return name
}

func nextSnapshotScheduleRun(previous time.Time, interval time.Duration, now time.Time) time.Time {
	if interval <= 0 {
		return now
	}
	if previous.IsZero() {
		return now.Add(interval)
	}
	if now.Before(previous) {
		return previous
	}

	steps := now.Sub(previous)/interval + 1
	return previous.Add(time.Duration(steps) * interval)
}

func validateSetSnapshotScheduleRequest(req SetSnapshotScheduleRequest) error {
	if req.Kind != SnapshotKindStandby && req.Kind != SnapshotKindStopped {
		return fmt.Errorf("%w: kind must be one of %s, %s", ErrInvalidRequest, SnapshotKindStandby, SnapshotKindStopped)
	}
	if req.Interval < minSnapshotScheduleInterval {
		return fmt.Errorf("%w: interval must be at least %s", ErrInvalidRequest, minSnapshotScheduleInterval)
	}
	if req.NamePrefix != "" {
		if err := validateInstanceName(req.NamePrefix); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
	}
	if err := tags.Validate(req.Metadata); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if req.Retention.MaxCount < 0 {
		return fmt.Errorf("%w: retention.max_count must be >= 0", ErrInvalidRequest)
	}
	if req.Retention.MaxAge < 0 {
		return fmt.Errorf("%w: retention.max_age must be >= 0", ErrInvalidRequest)
	}
	if req.Retention.MaxCount == 0 && req.Retention.MaxAge == 0 {
		return fmt.Errorf("%w: retention.max_count or retention.max_age must be set", ErrInvalidRequest)
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

	var stored snapshotScheduleStorage
	if err := json.Unmarshal(content, &stored); err != nil {
		return nil, fmt.Errorf("unmarshal snapshot schedule: %w", err)
	}

	interval, err := time.ParseDuration(stored.Interval)
	if err != nil {
		return nil, fmt.Errorf("parse schedule interval %q: %w", stored.Interval, err)
	}

	var maxAge time.Duration
	if stored.Retention.MaxAge != "" {
		maxAge, err = time.ParseDuration(stored.Retention.MaxAge)
		if err != nil {
			return nil, fmt.Errorf("parse schedule retention max_age %q: %w", stored.Retention.MaxAge, err)
		}
	}

	return &SnapshotSchedule{
		InstanceID:     stored.InstanceID,
		Kind:           stored.Kind,
		Interval:       interval,
		NamePrefix:     stored.NamePrefix,
		Metadata:       tags.Clone(stored.Metadata),
		Retention:      SnapshotScheduleRetention{MaxCount: stored.Retention.MaxCount, MaxAge: maxAge},
		NextRunAt:      stored.NextRunAt,
		LastRunAt:      stored.LastRunAt,
		LastSnapshotID: stored.LastSnapshotID,
		LastError:      stored.LastError,
		CreatedAt:      stored.CreatedAt,
		UpdatedAt:      stored.UpdatedAt,
	}, nil
}

func (m *manager) saveSnapshotScheduleUnlocked(schedule *SnapshotSchedule) error {
	if schedule == nil {
		return fmt.Errorf("nil snapshot schedule")
	}
	interval := schedule.Interval.String()
	maxAge := ""
	if schedule.Retention.MaxAge > 0 {
		maxAge = schedule.Retention.MaxAge.String()
	}

	stored := snapshotScheduleStorage{
		InstanceID:     schedule.InstanceID,
		Kind:           schedule.Kind,
		Interval:       interval,
		NamePrefix:     schedule.NamePrefix,
		Metadata:       tags.Clone(schedule.Metadata),
		Retention:      snapshotScheduleRetentionJSON{MaxCount: schedule.Retention.MaxCount, MaxAge: maxAge},
		NextRunAt:      schedule.NextRunAt.UTC(),
		LastRunAt:      schedule.LastRunAt,
		LastSnapshotID: schedule.LastSnapshotID,
		LastError:      schedule.LastError,
		CreatedAt:      schedule.CreatedAt.UTC(),
		UpdatedAt:      schedule.UpdatedAt.UTC(),
	}

	content, err := json.MarshalIndent(stored, "", "  ")
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
