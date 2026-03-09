package instances

import (
	"context"

	"github.com/kernel/hypeman/lib/scheduledsnapshots"
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
