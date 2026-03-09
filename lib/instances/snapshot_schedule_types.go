package instances

import (
	"context"
	"time"

	"github.com/kernel/hypeman/lib/tags"
)

// SnapshotScheduleRetention defines automatic cleanup rules for scheduled snapshots.
type SnapshotScheduleRetention struct {
	MaxCount int           // Keep at most this many scheduled snapshots for the instance (0 = unlimited)
	MaxAge   time.Duration // Delete scheduled snapshots older than this age (0 = unlimited)
}

// SnapshotSchedule defines periodic snapshot capture for a single instance.
type SnapshotSchedule struct {
	InstanceID     string
	Kind           SnapshotKind
	Interval       time.Duration
	NamePrefix     string
	Metadata       tags.Metadata
	Retention      SnapshotScheduleRetention
	NextRunAt      time.Time
	LastRunAt      *time.Time
	LastSnapshotID *string
	LastError      *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// SetSnapshotScheduleRequest configures or updates a schedule for an instance.
type SetSnapshotScheduleRequest struct {
	Kind       SnapshotKind
	Interval   time.Duration
	NamePrefix string
	Metadata   tags.Metadata
	Retention  SnapshotScheduleRetention
}

// SnapshotScheduleManager provides schedule operations in addition to core instance APIs.
type SnapshotScheduleManager interface {
	SetSnapshotSchedule(ctx context.Context, instanceID string, req SetSnapshotScheduleRequest) (*SnapshotSchedule, error)
	GetSnapshotSchedule(ctx context.Context, instanceID string) (*SnapshotSchedule, error)
	DeleteSnapshotSchedule(ctx context.Context, instanceID string) error
	RunSnapshotSchedules(ctx context.Context) error
}

var _ SnapshotScheduleManager = (*manager)(nil)
