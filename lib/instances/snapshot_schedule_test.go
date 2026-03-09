package instances

import (
	"context"
	"os"
	"testing"
	"time"

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
		Kind:       SnapshotKindStopped,
		Interval:   2 * time.Hour,
		NamePrefix: "nightly",
		Metadata:   map[string]string{"env": "test"},
		Retention: SnapshotScheduleRetention{
			MaxCount: 5,
		},
	})
	require.NoError(t, err)
	require.Equal(t, sourceID, schedule.InstanceID)
	require.Equal(t, SnapshotKindStopped, schedule.Kind)
	require.Equal(t, 2*time.Hour, schedule.Interval)
	require.Equal(t, "nightly", schedule.NamePrefix)
	require.Equal(t, 5, schedule.Retention.MaxCount)
	require.WithinDuration(t, time.Now().UTC().Add(2*time.Hour), schedule.NextRunAt, 3*time.Second)

	loaded, err := mgr.GetSnapshotSchedule(ctx, sourceID)
	require.NoError(t, err)
	assert.Equal(t, schedule.InstanceID, loaded.InstanceID)
	assert.Equal(t, schedule.Kind, loaded.Kind)
	assert.Equal(t, schedule.Interval, loaded.Interval)
	assert.Equal(t, schedule.Retention.MaxCount, loaded.Retention.MaxCount)
	assert.Equal(t, "test", loaded.Metadata["env"])

	require.NoError(t, mgr.DeleteSnapshotSchedule(ctx, sourceID))

	_, err = mgr.GetSnapshotSchedule(ctx, sourceID)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSnapshotScheduleNotFound)
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
			snapshotScheduleMetadataKey:        "true",
			snapshotScheduleMetadataInstanceID: sourceID,
		},
	})
	require.NoError(t, err)

	time.Sleep(1100 * time.Millisecond)

	newer, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStopped,
		Name: "newer-scheduled",
		Metadata: map[string]string{
			snapshotScheduleMetadataKey:        "true",
			snapshotScheduleMetadataInstanceID: sourceID,
		},
	})
	require.NoError(t, err)

	_, err = mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStopped,
		Name: "manual-snapshot",
	})
	require.NoError(t, err)

	_, err = mgr.SetSnapshotSchedule(ctx, sourceID, SetSnapshotScheduleRequest{
		Kind:       SnapshotKindStopped,
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
		if isScheduledSnapshot(snapshot, sourceID) {
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
		Kind:     SnapshotKindStopped,
		Interval: time.Hour,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
}

func TestDeleteInstanceRemovesSnapshotSchedule(t *testing.T) {
	t.Parallel()
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-schedule-delete-src"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, "snapshot-schedule-delete-src", hvType)

	_, err := mgr.SetSnapshotSchedule(ctx, sourceID, SetSnapshotScheduleRequest{
		Kind:     SnapshotKindStopped,
		Interval: time.Hour,
		Retention: SnapshotScheduleRetention{
			MaxCount: 3,
		},
	})
	require.NoError(t, err)
	require.FileExists(t, mgr.paths.InstanceSnapshotSchedule(sourceID))

	require.NoError(t, mgr.DeleteInstance(ctx, sourceID))

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
		Kind:     SnapshotKindStandby, // Invalid against Stopped source state, should fail during run.
		Interval: time.Hour,
		Retention: SnapshotScheduleRetention{
			MaxCount: 1,
		},
	})
	require.NoError(t, err)
	_, err = mgr.SetSnapshotSchedule(ctx, sourceB, SetSnapshotScheduleRequest{
		Kind:     SnapshotKindStandby, // Invalid against Stopped source state, should fail during run.
		Interval: time.Hour,
		Retention: SnapshotScheduleRetention{
			MaxCount: 1,
		},
	})
	require.NoError(t, err)

	markSnapshotScheduleDue(t, mgr, sourceA)
	markSnapshotScheduleDue(t, mgr, sourceB)

	err = mgr.RunSnapshotSchedules(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), sourceA)
	assert.Contains(t, err.Error(), sourceB)
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
