package devices

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetVFHealthStore points the package-level store at a fresh temp file and
// restores an empty, unpersisted store when the test finishes.
func resetVFHealthStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vf-health.json")
	require.NoError(t, initVFHealthStore(path))
	t.Cleanup(func() {
		vfHealth.mu.Lock()
		defer vfHealth.mu.Unlock()
		vfHealth.path = ""
		vfHealth.records = make(map[string]VFHealthRecord)
	})
	return path
}

func TestQuarantineVFPersistsAcrossReload(t *testing.T) {
	path := resetVFHealthStore(t)

	record, err := QuarantineVF(VFQuarantine{
		VFAddress:    "0000:e3:00.4",
		InstanceID:   "instance-1",
		SentinelLine: "NVRM: GPU 0000:e3:00.4: RmInitAdapter failed! (0x22:0x65:884)",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, record.WedgeCount)
	assert.False(t, record.QuarantinedAt.IsZero())

	// A repeat conviction increments the count and keeps the original time.
	again, err := QuarantineVF(VFQuarantine{VFAddress: "0000:e3:00.4", InstanceID: "instance-2"})
	require.NoError(t, err)
	assert.Equal(t, 2, again.WedgeCount)
	assert.Equal(t, record.QuarantinedAt, again.QuarantinedAt)

	// Reload from disk, as a hypeman restart would.
	require.NoError(t, initVFHealthStore(path))
	records := QuarantinedVFs()
	require.Len(t, records, 1)
	assert.Equal(t, "0000:e3:00.4", records[0].VFAddress)
	assert.Equal(t, 2, records[0].WedgeCount)
	assert.Equal(t, "instance-2", records[0].InstanceID)
}

func TestClearVFQuarantine(t *testing.T) {
	resetVFHealthStore(t)

	_, err := QuarantineVF(VFQuarantine{VFAddress: "0000:e3:00.4"})
	require.NoError(t, err)

	cleared, err := ClearVFQuarantine("0000:e3:00.4")
	require.NoError(t, err)
	assert.True(t, cleared)
	assert.Empty(t, QuarantinedVFs())

	cleared, err = ClearVFQuarantine("0000:e3:00.4")
	require.NoError(t, err)
	assert.False(t, cleared)
}
