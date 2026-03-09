package scheduledsnapshots

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateSetRequestNamePrefixLimit(t *testing.T) {
	validateName := func(name string) error {
		if name == "" {
			return assert.AnError
		}
		return nil
	}

	req := SetRequest{
		Interval:   time.Hour,
		NamePrefix: strings.Repeat("a", MaxNamePrefixLength+1),
		Retention: Retention{
			MaxCount: 1,
		},
	}
	err := ValidateSetRequest(req, validateName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name_prefix must be at most")
}

func TestMarshalUnmarshalScheduleRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	last := now.Add(-time.Hour)
	id := "snap-1"
	in := &Schedule{
		InstanceID:     "inst-1",
		Interval:       2 * time.Hour,
		NamePrefix:     "nightly",
		Metadata:       map[string]string{"env": "test"},
		Retention:      Retention{MaxCount: 3, MaxAge: 24 * time.Hour},
		NextRunAt:      now,
		LastRunAt:      &last,
		LastSnapshotID: &id,
		CreatedAt:      now.Add(-2 * time.Hour),
		UpdatedAt:      now,
	}

	raw, err := MarshalSchedule(in)
	require.NoError(t, err)

	out, err := UnmarshalSchedule(raw)
	require.NoError(t, err)
	assert.Equal(t, in.InstanceID, out.InstanceID)
	assert.Equal(t, in.Interval, out.Interval)
	assert.Equal(t, in.NamePrefix, out.NamePrefix)
	assert.Equal(t, in.Retention.MaxCount, out.Retention.MaxCount)
	assert.Equal(t, in.Retention.MaxAge, out.Retention.MaxAge)
	require.NotNil(t, out.LastRunAt)
	assert.Equal(t, *in.LastRunAt, *out.LastRunAt)
	require.NotNil(t, out.LastSnapshotID)
	assert.Equal(t, *in.LastSnapshotID, *out.LastSnapshotID)
}

func TestMarshalSchedulePersistsZeroMaxCount(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	in := &Schedule{
		InstanceID: "inst-1",
		Interval:   time.Hour,
		Retention: Retention{
			MaxCount: 0,
			MaxAge:   24 * time.Hour,
		},
		NextRunAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}

	raw, err := MarshalSchedule(in)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "\"max_count\": 0")

	out, err := UnmarshalSchedule(raw)
	require.NoError(t, err)
	assert.Equal(t, 0, out.Retention.MaxCount)
	assert.Equal(t, 24*time.Hour, out.Retention.MaxAge)
}
