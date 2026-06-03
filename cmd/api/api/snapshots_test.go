package api

import (
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/instances"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotScheduleToOAPIPreservesZeroMaxCount(t *testing.T) {
	t.Parallel()

	schedule := instances.SnapshotSchedule{
		InstanceID: "inst-1",
		Interval:   time.Hour,
		Retention: instances.SnapshotScheduleRetention{
			MaxCount: 0,
			MaxAge:   24 * time.Hour,
		},
		NextRunAt: time.Now().UTC().Add(time.Hour),
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	out := snapshotScheduleToOAPI(schedule)
	require.NotNil(t, out.Retention.MaxCount)
	assert.Equal(t, 0, *out.Retention.MaxCount)
	require.NotNil(t, out.Retention.MaxAge)
	assert.Equal(t, "24h0m0s", *out.Retention.MaxAge)
}
