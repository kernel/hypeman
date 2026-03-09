package instances

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/scheduledsnapshots"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotScheduleSetGetDelete(t *testing.T) {
	t.Parallel()
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-schedule-src"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, "snapshot-schedule-src", hvType)

	schedule, err := mgr.SetSnapshotSchedule(ctx, sourceID, SetSnapshotScheduleRequest{
		Interval:   2 * time.Hour,
		NamePrefix: "nightly",
		Metadata:   map[string]string{"env": "test"},
		Retention: SnapshotScheduleRetention{
			MaxCount: 5,
		},
	})
	require.NoError(t, err)
	require.Equal(t, sourceID, schedule.InstanceID)
	require.Equal(t, 2*time.Hour, schedule.Interval)
	require.Equal(t, "nightly", schedule.NamePrefix)
	require.Equal(t, 5, schedule.Retention.MaxCount)
	require.WithinDuration(t, time.Now().UTC().Add(2*time.Hour), schedule.NextRunAt, 3*time.Second)

	loaded, err := mgr.GetSnapshotSchedule(ctx, sourceID)
	require.NoError(t, err)
	assert.Equal(t, schedule.InstanceID, loaded.InstanceID)
	assert.Equal(t, schedule.Interval, loaded.Interval)
	assert.Equal(t, schedule.Retention.MaxCount, loaded.Retention.MaxCount)
	assert.Equal(t, "test", loaded.Metadata["env"])

	require.NoError(t, mgr.DeleteSnapshotSchedule(ctx, sourceID))

	_, err = mgr.GetSnapshotSchedule(ctx, sourceID)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSnapshotScheduleNotFound)
}

func TestSnapshotScheduleUpdatePreservesOperationalHistory(t *testing.T) {
	t.Parallel()
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-schedule-update-history-src"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, sourceID, hvType)

	created, err := mgr.SetSnapshotSchedule(ctx, sourceID, SetSnapshotScheduleRequest{
		Interval: time.Hour,
		Retention: SnapshotScheduleRetention{
			MaxCount: 2,
		},
	})
	require.NoError(t, err)

	expectedLastRunAt := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	expectedLastSnapshotID := "snapshot-123"
	expectedLastError := "last run failed"

	lock := mgr.getInstanceLock(sourceID)
	lock.Lock()
	schedule, err := mgr.getSnapshotScheduleUnlocked(sourceID)
	require.NoError(t, err)
	schedule.LastRunAt = &expectedLastRunAt
	schedule.LastSnapshotID = &expectedLastSnapshotID
	schedule.LastError = &expectedLastError
	require.NoError(t, mgr.saveSnapshotScheduleUnlocked(schedule))
	lock.Unlock()

	updated, err := mgr.SetSnapshotSchedule(ctx, sourceID, SetSnapshotScheduleRequest{
		Interval:   2 * time.Hour,
		NamePrefix: "nightly",
		Retention: SnapshotScheduleRetention{
			MaxCount: 5,
		},
	})
	require.NoError(t, err)

	require.NotNil(t, updated.LastRunAt)
	assert.Equal(t, expectedLastRunAt, *updated.LastRunAt)
	require.NotNil(t, updated.LastSnapshotID)
	assert.Equal(t, expectedLastSnapshotID, *updated.LastSnapshotID)
	require.NotNil(t, updated.LastError)
	assert.Equal(t, expectedLastError, *updated.LastError)
	assert.Equal(t, created.CreatedAt, updated.CreatedAt)
}

func TestSnapshotScheduleUsesStoppedSnapshotWhenSourceIsStopped(t *testing.T) {
	t.Parallel()
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-schedule-auto-kind-stopped-src"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, sourceID, hvType)

	_, err := mgr.SetSnapshotSchedule(ctx, sourceID, SetSnapshotScheduleRequest{
		Interval: time.Hour,
		Retention: SnapshotScheduleRetention{
			MaxCount: 1,
		},
	})
	require.NoError(t, err)

	markSnapshotScheduleDue(t, mgr, sourceID)
	require.NoError(t, mgr.RunSnapshotSchedules(ctx))

	snaps, err := mgr.ListSnapshots(ctx, &ListSnapshotsFilter{SourceInstanceID: &sourceID})
	require.NoError(t, err)

	foundScheduled := false
	for _, snapshot := range snaps {
		if !scheduledsnapshots.IsScheduledSnapshot(snapshot.Metadata, sourceID) {
			continue
		}
		foundScheduled = true
		assert.Equal(t, SnapshotKindStopped, snapshot.Kind)
	}
	require.True(t, foundScheduled, "expected at least one scheduled snapshot")
}

func TestRunSnapshotSchedulesCreatesSnapshotAndAppliesRetention(t *testing.T) {
	t.Parallel()
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-schedule-run-src"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, "snapshot-schedule-run-src", hvType)

	older, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStopped,
		Name: "older-scheduled",
		Metadata: map[string]string{
			scheduledsnapshots.MetadataKeyScheduled:        "true",
			scheduledsnapshots.MetadataKeySourceInstanceID: sourceID,
		},
	})
	require.NoError(t, err)

	time.Sleep(1100 * time.Millisecond)

	newer, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStopped,
		Name: "newer-scheduled",
		Metadata: map[string]string{
			scheduledsnapshots.MetadataKeyScheduled:        "true",
			scheduledsnapshots.MetadataKeySourceInstanceID: sourceID,
		},
	})
	require.NoError(t, err)

	_, err = mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStopped,
		Name: "manual-snapshot",
	})
	require.NoError(t, err)

	_, err = mgr.SetSnapshotSchedule(ctx, sourceID, SetSnapshotScheduleRequest{
		Interval:   time.Hour,
		NamePrefix: "nightly",
		Retention: SnapshotScheduleRetention{
			MaxCount: 2,
		},
	})
	require.NoError(t, err)

	markSnapshotScheduleDue(t, mgr, sourceID)

	require.NoError(t, mgr.RunSnapshotSchedules(ctx))

	snaps, err := mgr.ListSnapshots(ctx, &ListSnapshotsFilter{SourceInstanceID: &sourceID})
	require.NoError(t, err)

	scheduledIDs := make(map[string]struct{})
	manualCount := 0
	for _, snapshot := range snaps {
		if scheduledsnapshots.IsScheduledSnapshot(snapshot.Metadata, sourceID) {
			scheduledIDs[snapshot.Id] = struct{}{}
		} else {
			manualCount++
		}
	}

	assert.Len(t, scheduledIDs, 2, "retention should keep only two scheduled snapshots")
	assert.NotContains(t, scheduledIDs, older.Id, "oldest scheduled snapshot should be cleaned up")
	assert.Contains(t, scheduledIDs, newer.Id, "newer pre-existing scheduled snapshot should be retained")
	assert.Equal(t, 1, manualCount, "manual snapshots must not be auto-cleaned")

	schedule, err := mgr.GetSnapshotSchedule(ctx, sourceID)
	require.NoError(t, err)
	require.NotNil(t, schedule.LastRunAt)
	require.NotNil(t, schedule.LastSnapshotID)
	assert.Nil(t, schedule.LastError)
}

func TestSnapshotScheduleRequiresRetentionPolicy(t *testing.T) {
	t.Parallel()
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-schedule-validate-src"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, "snapshot-schedule-validate-src", hvType)

	_, err := mgr.SetSnapshotSchedule(ctx, sourceID, SetSnapshotScheduleRequest{
		Interval: time.Hour,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
}

func TestSnapshotScheduleNamePrefixLengthValidation(t *testing.T) {
	t.Parallel()
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-schedule-prefix-src"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, sourceID, hvType)

	tooLong := strings.Repeat("a", scheduledsnapshots.MaxNamePrefixLength+1)
	_, err := mgr.SetSnapshotSchedule(ctx, sourceID, SetSnapshotScheduleRequest{
		Interval:   time.Hour,
		NamePrefix: tooLong,
		Retention: SnapshotScheduleRetention{
			MaxCount: 1,
		},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
	assert.Contains(t, err.Error(), "name_prefix must be at most")

	atLimit := strings.Repeat("a", scheduledsnapshots.MaxNamePrefixLength)
	_, err = mgr.SetSnapshotSchedule(ctx, sourceID, SetSnapshotScheduleRequest{
		Interval:   time.Hour,
		NamePrefix: atLimit,
		Retention: SnapshotScheduleRetention{
			MaxCount: 1,
		},
	})
	require.NoError(t, err)
}

func TestDeleteInstanceKeepsScheduleUntilScheduledSnapshotsAreGone(t *testing.T) {
	t.Parallel()
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-schedule-delete-src"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, "snapshot-schedule-delete-src", hvType)

	_, err := mgr.SetSnapshotSchedule(ctx, sourceID, SetSnapshotScheduleRequest{
		Interval: time.Hour,
		Retention: SnapshotScheduleRetention{
			MaxAge: 24 * time.Hour,
		},
	})
	require.NoError(t, err)

	scheduledSnapshot, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStopped,
		Name: "scheduled-before-delete",
		Metadata: map[string]string{
			scheduledsnapshots.MetadataKeyScheduled:        "true",
			scheduledsnapshots.MetadataKeySourceInstanceID: sourceID,
		},
	})
	require.NoError(t, err)
	require.FileExists(t, mgr.paths.InstanceSnapshotSchedule(sourceID))

	require.NoError(t, mgr.DeleteInstance(ctx, sourceID))
	require.FileExists(t, mgr.paths.InstanceSnapshotSchedule(sourceID))

	markSnapshotScheduleDue(t, mgr, sourceID)
	require.NoError(t, mgr.RunSnapshotSchedules(ctx))
	require.FileExists(t, mgr.paths.InstanceSnapshotSchedule(sourceID))

	require.NoError(t, mgr.DeleteSnapshot(ctx, scheduledSnapshot.Id))

	markSnapshotScheduleDue(t, mgr, sourceID)
	require.NoError(t, mgr.RunSnapshotSchedules(ctx))

	_, statErr := os.Stat(mgr.paths.InstanceSnapshotSchedule(sourceID))
	require.Error(t, statErr)
	assert.True(t, os.IsNotExist(statErr))
}

func TestDeleteInstanceWithCountOnlyRetentionRemovesConvergedSchedule(t *testing.T) {
	t.Parallel()
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-schedule-delete-count-only-src"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, sourceID, hvType)

	_, err := mgr.SetSnapshotSchedule(ctx, sourceID, SetSnapshotScheduleRequest{
		Interval: time.Hour,
		Retention: SnapshotScheduleRetention{
			MaxCount: 3,
		},
	})
	require.NoError(t, err)

	_, err = mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStopped,
		Name: "scheduled-before-delete-count-only",
		Metadata: map[string]string{
			scheduledsnapshots.MetadataKeyScheduled:        "true",
			scheduledsnapshots.MetadataKeySourceInstanceID: sourceID,
		},
	})
	require.NoError(t, err)

	require.NoError(t, mgr.DeleteInstance(ctx, sourceID))
	require.FileExists(t, mgr.paths.InstanceSnapshotSchedule(sourceID))

	markSnapshotScheduleDue(t, mgr, sourceID)
	require.NoError(t, mgr.RunSnapshotSchedules(ctx))

	_, statErr := os.Stat(mgr.paths.InstanceSnapshotSchedule(sourceID))
	require.Error(t, statErr)
	assert.True(t, os.IsNotExist(statErr))
}

func TestRunSnapshotSchedulesAggregatesErrorsAcrossInstances(t *testing.T) {
	t.Parallel()
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceA := "snapshot-schedule-error-src-a"
	sourceB := "snapshot-schedule-error-src-b"
	createStoppedSnapshotSourceFixture(t, mgr, sourceA, sourceA, hvType)
	createStoppedSnapshotSourceFixture(t, mgr, sourceB, sourceB, hvType)

	_, err := mgr.SetSnapshotSchedule(ctx, sourceA, SetSnapshotScheduleRequest{
		Interval: time.Hour,
		Retention: SnapshotScheduleRetention{
			MaxCount: 1,
		},
	})
	require.NoError(t, err)
	_, err = mgr.SetSnapshotSchedule(ctx, sourceB, SetSnapshotScheduleRequest{
		Interval: time.Hour,
		Retention: SnapshotScheduleRetention{
			MaxCount: 1,
		},
	})
	require.NoError(t, err)

	markSnapshotScheduleDue(t, mgr, sourceA)
	markSnapshotScheduleDue(t, mgr, sourceB)

	require.NoError(t, os.WriteFile(mgr.paths.InstanceSnapshotSchedule(sourceA), []byte("{"), 0644))
	require.NoError(t, os.WriteFile(mgr.paths.InstanceSnapshotSchedule(sourceB), []byte("{"), 0644))

	err = mgr.RunSnapshotSchedules(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), sourceA)
	assert.Contains(t, err.Error(), sourceB)
}

func TestRunSnapshotSchedulesStopsOnCanceledContext(t *testing.T) {
	t.Parallel()
	mgr, _ := setupTestManager(t)

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-schedule-canceled-context-src"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, sourceID, hvType)

	_, err := mgr.SetSnapshotSchedule(context.Background(), sourceID, SetSnapshotScheduleRequest{
		Interval: time.Hour,
		Retention: SnapshotScheduleRetention{
			MaxCount: 1,
		},
	})
	require.NoError(t, err)

	markSnapshotScheduleDue(t, mgr, sourceID)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = mgr.RunSnapshotSchedules(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)

	snapshots, err := mgr.ListSnapshots(context.Background(), &ListSnapshotsFilter{SourceInstanceID: &sourceID})
	require.NoError(t, err)

	for _, snapshot := range snapshots {
		assert.False(t, scheduledsnapshots.IsScheduledSnapshot(snapshot.Metadata, sourceID), "canceled context should prevent scheduled snapshot runs")
	}
}

func markSnapshotScheduleDue(t *testing.T, mgr *manager, instanceID string) {
	t.Helper()
	lock := mgr.getInstanceLock(instanceID)
	lock.Lock()
	defer lock.Unlock()

	schedule, err := mgr.getSnapshotScheduleUnlocked(instanceID)
	require.NoError(t, err)
	schedule.NextRunAt = time.Now().UTC().Add(-time.Minute)
	schedule.UpdatedAt = time.Now().UTC()
	require.NoError(t, mgr.saveSnapshotScheduleUnlocked(schedule))
}
