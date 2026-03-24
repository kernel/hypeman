package scheduledsnapshots

import (
	"time"

	"github.com/kernel/hypeman/lib/tags"
)

const (
	MetadataKeyScheduled        = "hypeman.scheduled"
	MetadataKeySourceInstanceID = "hypeman.schedule_instance_id"
	DefaultNamePrefix           = "scheduled"
	NameTimestampFormat         = "20060102-150405"
	MaxSnapshotNameLength       = 63
	MaxNamePrefixLength         = MaxSnapshotNameLength - len(NameTimestampFormat) - 1
	MinInterval                 = time.Minute
	maxCadenceJitter            = 5 * time.Minute
)

// Retention defines automatic cleanup rules for scheduled snapshots.
type Retention struct {
	MaxCount int           // Keep at most this many scheduled snapshots for the instance (0 = unlimited)
	MaxAge   time.Duration // Delete scheduled snapshots older than this age (0 = unlimited)
}

// Schedule defines periodic snapshot capture for a single instance.
type Schedule struct {
	InstanceID     string
	Interval       time.Duration
	NamePrefix     string
	Metadata       tags.Tags
	Retention      Retention
	NextRunAt      time.Time
	LastRunAt      *time.Time
	LastSnapshotID *string
	LastError      *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// SetRequest configures or updates a schedule for an instance.
type SetRequest struct {
	Interval   time.Duration
	NamePrefix string
	Metadata   tags.Tags
	Retention  Retention
}
