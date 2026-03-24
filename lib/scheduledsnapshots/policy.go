package scheduledsnapshots

import (
	"fmt"
	"hash/fnv"
	"time"

	"github.com/kernel/hypeman/lib/tags"
)

func ValidateSetRequest(req SetRequest, validateName func(name string) error) error {
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

func BuildSnapshotMetadata(instanceID string, userMetadata tags.Tags) tags.Tags {
	metadata := tags.Clone(userMetadata)
	if metadata == nil {
		metadata = make(tags.Tags)
	}
	metadata[MetadataKeyScheduled] = "true"
	metadata[MetadataKeySourceInstanceID] = instanceID
	return metadata
}

func IsScheduledSnapshot(metadata tags.Tags, instanceID string) bool {
	if metadata == nil {
		return false
	}
	if metadata[MetadataKeyScheduled] != "true" {
		return false
	}
	return metadata[MetadataKeySourceInstanceID] == instanceID
}

func BuildSnapshotName(prefix string, runAt time.Time) string {
	if prefix == "" {
		prefix = DefaultNamePrefix
	}
	return prefix + "-" + runAt.UTC().Format(NameTimestampFormat)
}

func InitialNextRunAt(instanceID string, interval time.Duration, now time.Time) time.Time {
	return now.UTC().Add(interval).Add(cadenceJitter(instanceID, interval))
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

	steps := int64(now.Sub(previous)/interval) + 1
	return previous.Add(time.Duration(steps) * interval)
}

func cadenceJitter(instanceID string, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}

	maxJitter := interval / 10
	if maxJitter > maxCadenceJitter {
		maxJitter = maxCadenceJitter
	}
	if maxJitter <= 0 {
		return 0
	}

	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(instanceID))
	return time.Duration(hasher.Sum64() % uint64(maxJitter))
}
