package scheduledsnapshots

import (
	"fmt"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/snapshot"
	"github.com/kernel/hypeman/lib/tags"
)

func NormalizeSetRequest(req SetRequest) SetRequest {
	if req.Kind == "" {
		req.Kind = DefaultKind
	}
	return req
}

func ValidateSetRequest(req SetRequest, validateName func(name string) error) error {
	if req.Kind != snapshot.SnapshotKindStandby && req.Kind != snapshot.SnapshotKindStopped {
		return fmt.Errorf("kind must be one of %s, %s", snapshot.SnapshotKindStandby, snapshot.SnapshotKindStopped)
	}
	if req.Interval < MinInterval {
		return fmt.Errorf("interval must be at least %s", MinInterval)
	}
	if req.NamePrefix != "" {
		if validateName == nil {
			return fmt.Errorf("name validator is required")
		}
		if err := validateName(req.NamePrefix); err != nil {
			return err
		}
		if len(req.NamePrefix) > MaxNamePrefixLength {
			return fmt.Errorf("name_prefix must be at most %d characters", MaxNamePrefixLength)
		}
	}
	if err := tags.Validate(req.Metadata); err != nil {
		return err
	}
	if req.Retention.MaxCount < 0 {
		return fmt.Errorf("retention.max_count must be >= 0")
	}
	if req.Retention.MaxAge < 0 {
		return fmt.Errorf("retention.max_age must be >= 0")
	}
	if req.Retention.MaxCount == 0 && req.Retention.MaxAge == 0 {
		return fmt.Errorf("retention.max_count or retention.max_age must be set")
	}
	return nil
}

func BuildSnapshotMetadata(instanceID string, userMetadata tags.Metadata) tags.Metadata {
	metadata := tags.Clone(userMetadata)
	if metadata == nil {
		metadata = make(tags.Metadata)
	}
	metadata[MetadataKeyScheduled] = "true"
	metadata[MetadataKeySourceInstanceID] = instanceID
	return metadata
}

func IsScheduledSnapshot(metadata tags.Metadata, instanceID string) bool {
	if metadata == nil {
		return false
	}
	if metadata[MetadataKeyScheduled] != "true" {
		return false
	}
	return metadata[MetadataKeySourceInstanceID] == instanceID
}

func BuildSnapshotName(prefix string, runAt time.Time, validateName func(name string) error) string {
	if prefix == "" {
		prefix = DefaultNamePrefix
	}

	suffix := runAt.UTC().Format(NameTimestampFormat)
	if len(prefix) > MaxNamePrefixLength {
		prefix = strings.Trim(prefix[:MaxNamePrefixLength], "-")
		if prefix == "" {
			prefix = "s"
		}
	}

	name := prefix + "-" + suffix
	if validateName != nil {
		if err := validateName(name); err != nil {
			return "s-" + suffix
		}
	}
	return name
}

func NextRun(previous time.Time, interval time.Duration, now time.Time) time.Time {
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
