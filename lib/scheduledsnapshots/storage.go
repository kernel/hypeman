package scheduledsnapshots

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/tags"
)

type storageModel struct {
	InstanceID     string           `json:"instance_id"`
	Interval       string           `json:"interval"`
	NamePrefix     string           `json:"name_prefix,omitempty"`
	Metadata       tags.Metadata    `json:"metadata,omitempty"`
	Retention      retentionStorage `json:"retention"`
	NextRunAt      time.Time        `json:"next_run_at"`
	LastRunAt      *time.Time       `json:"last_run_at,omitempty"`
	LastSnapshotID *string          `json:"last_snapshot_id,omitempty"`
	LastError      *string          `json:"last_error,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

type retentionStorage struct {
	MaxCount int    `json:"max_count"`
	MaxAge   string `json:"max_age,omitempty"`
}

func MarshalSchedule(schedule *Schedule) ([]byte, error) {
	if schedule == nil {
		return nil, fmt.Errorf("nil snapshot schedule")
	}
	maxAge := ""
	if schedule.Retention.MaxAge > 0 {
		maxAge = schedule.Retention.MaxAge.String()
	}

	model := storageModel{
		InstanceID:     schedule.InstanceID,
		Interval:       schedule.Interval.String(),
		NamePrefix:     schedule.NamePrefix,
		Metadata:       tags.Clone(schedule.Metadata),
		Retention:      retentionStorage{MaxCount: schedule.Retention.MaxCount, MaxAge: maxAge},
		NextRunAt:      schedule.NextRunAt.UTC(),
		LastRunAt:      schedule.LastRunAt,
		LastSnapshotID: schedule.LastSnapshotID,
		LastError:      schedule.LastError,
		CreatedAt:      schedule.CreatedAt.UTC(),
		UpdatedAt:      schedule.UpdatedAt.UTC(),
	}
	return json.MarshalIndent(model, "", "  ")
}

func UnmarshalSchedule(content []byte) (*Schedule, error) {
	var model storageModel
	if err := json.Unmarshal(content, &model); err != nil {
		return nil, fmt.Errorf("unmarshal snapshot schedule: %w", err)
	}

	interval, err := time.ParseDuration(model.Interval)
	if err != nil {
		return nil, fmt.Errorf("parse schedule interval %q: %w", model.Interval, err)
	}

	var maxAge time.Duration
	if model.Retention.MaxAge != "" {
		maxAge, err = time.ParseDuration(model.Retention.MaxAge)
		if err != nil {
			return nil, fmt.Errorf("parse schedule retention max_age %q: %w", model.Retention.MaxAge, err)
		}
	}

	return &Schedule{
		InstanceID:     model.InstanceID,
		Interval:       interval,
		NamePrefix:     model.NamePrefix,
		Metadata:       tags.Clone(model.Metadata),
		Retention:      Retention{MaxCount: model.Retention.MaxCount, MaxAge: maxAge},
		NextRunAt:      model.NextRunAt,
		LastRunAt:      model.LastRunAt,
		LastSnapshotID: model.LastSnapshotID,
		LastError:      model.LastError,
		CreatedAt:      model.CreatedAt,
		UpdatedAt:      model.UpdatedAt,
	}, nil
}

func ListInstanceIDs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read snapshot schedules directory: %w", err)
	}

	instanceIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		instanceID := strings.TrimSuffix(name, ".json")
		if instanceID == "" {
			continue
		}
		instanceIDs = append(instanceIDs, instanceID)
	}
	sort.Strings(instanceIDs)
	return instanceIDs, nil
}
