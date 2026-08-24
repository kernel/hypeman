package devices

import (
	"os"
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
		vfHealth.loadErr = nil
	})
	return path
}

func TestQuarantineVFPersistsAcrossReload(t *testing.T) {
	path := resetVFHealthStore(t)

	existed, err := QuarantineVF(VFQuarantine{
		VFAddress:    "0000:e3:00.4",
		InstanceID:   "instance-1",
		SentinelLine: "HYPEMAN-GPU-INIT-FAILED ts=2026-08-20T15:04:05Z nvrm=\"NVRM: GPU 0000:e3:00.4: RmInitAdapter failed! (0x22:0x65:884)\"",
	})
	require.NoError(t, err)
	assert.False(t, existed)
	initial := QuarantinedVFs()
	require.Len(t, initial, 1)
	assert.False(t, initial[0].QuarantinedAt.IsZero())

	// A repeat conviction (another victim boot, or a rescan after restart)
	// leaves the original record unchanged: one wedge, one record.
	existed, err = QuarantineVF(VFQuarantine{VFAddress: "0000:e3:00.4", InstanceID: "instance-2"})
	require.NoError(t, err)
	assert.True(t, existed)
	assert.Equal(t, initial, QuarantinedVFs())

	// Reload from disk, as a hypeman restart would.
	require.NoError(t, initVFHealthStore(path))
	records := QuarantinedVFs()
	require.Len(t, records, 1)
	assert.Equal(t, "0000:e3:00.4", records[0].VFAddress)
	assert.Equal(t, "instance-1", records[0].InstanceID)
}

func TestQuarantineVFRejectsInvalidAddress(t *testing.T) {
	resetVFHealthStore(t)

	_, err := QuarantineVF(VFQuarantine{VFAddress: "not-a-pci-address"})
	require.ErrorContains(t, err, "invalid VF address")
	assert.Empty(t, QuarantinedVFs())
}

func TestQuarantineVFRollsBackOnPersistFailure(t *testing.T) {
	resetVFHealthStore(t)
	// Point the store below a path component that is a file, so persisting
	// fails at MkdirAll.
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, nil, 0644))
	vfHealth.path = filepath.Join(blocker, "vf-health.json")

	_, err := QuarantineVF(VFQuarantine{VFAddress: "0000:e3:00.4"})
	require.Error(t, err)

	// A quarantine is only real once persisted: the failed record must not
	// linger in memory, or the retried conviction would be treated as a
	// repeat and never reach disk.
	assert.Empty(t, QuarantinedVFs())
}

func TestCheckedAddressesFailsClosedOnUnloadedState(t *testing.T) {
	path := resetVFHealthStore(t)

	_, err := QuarantineVF(VFQuarantine{VFAddress: "0000:e3:00.4"})
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(path, []byte("not json"), 0644))
	require.Error(t, initVFHealthStore(path))

	// Placement must not run against a record set that is only empty because
	// the state file was unreadable.
	_, err = vfHealth.checkedAddresses()
	require.Error(t, err)

	// A repaired file self-heals on the next placement attempt.
	restored := `[{"vf_address":"0000:e3:00.4","quarantined_at":"2026-08-20T00:00:00Z"}]`
	require.NoError(t, os.WriteFile(path, []byte(restored), 0644))
	addresses, err := vfHealth.checkedAddresses()
	require.NoError(t, err)
	assert.Contains(t, addresses, "0000:e3:00.4")
}

func TestCheckedAddressesFailsClosedOnInvalidRecord(t *testing.T) {
	tests := []struct {
		name    string
		state   string
		wantErr string
	}{
		{
			name:    "null state",
			state:   `null`,
			wantErr: "expected an array",
		},
		{
			name:    "missing fields",
			state:   `[{}]`,
			wantErr: "invalid VF address",
		},
		{
			name:    "invalid address",
			state:   `[{"vf_address":"not-a-pci-address","quarantined_at":"2026-08-20T00:00:00Z"}]`,
			wantErr: "invalid VF address",
		},
		{
			name:    "missing timestamp",
			state:   `[{"vf_address":"0000:e3:00.4"}]`,
			wantErr: "missing quarantine timestamp",
		},
		{
			name:    "duplicate address",
			state:   `[{"vf_address":"0000:e3:00.4","quarantined_at":"2026-08-20T00:00:00Z"},{"vf_address":"0000:e3:00.4","quarantined_at":"2026-08-21T00:00:00Z"}]`,
			wantErr: "duplicate VF address",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := resetVFHealthStore(t)
			require.NoError(t, os.WriteFile(path, []byte(tt.state), 0644))
			require.ErrorContains(t, initVFHealthStore(path), tt.wantErr)
			assert.True(t, VFHealthStoreUnavailable())
			assert.Empty(t, QuarantinedVFs())

			_, err := vfHealth.checkedAddresses()
			require.Error(t, err)
		})
	}
}

func TestQuarantineVFRefusesToClobberUnloadedState(t *testing.T) {
	path := resetVFHealthStore(t)

	_, err := QuarantineVF(VFQuarantine{VFAddress: "0000:e3:00.4"})
	require.NoError(t, err)

	// Corrupt the state file and reload, as a hypeman restart over a bad
	// file would. The load fails and mutations must not persist the empty
	// in-memory store over the previous quarantines.
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0644))
	require.Error(t, initVFHealthStore(path))

	_, err = QuarantineVF(VFQuarantine{VFAddress: "0000:e3:00.5"})
	require.Error(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "not json", string(data), "a failed load must not be overwritten by later convictions")

	// Once the file is readable again the next conviction self-heals: it
	// reloads the persisted records and appends to them.
	restored := `[{"vf_address":"0000:e3:00.4","quarantined_at":"2026-08-20T00:00:00Z"}]`
	require.NoError(t, os.WriteFile(path, []byte(restored), 0644))
	existed, err := QuarantineVF(VFQuarantine{VFAddress: "0000:e3:00.5"})
	require.NoError(t, err)
	assert.False(t, existed)
	records := QuarantinedVFs()
	require.Len(t, records, 2, "reload must recover the previously persisted quarantine")
	assert.Equal(t, "0000:e3:00.4", records[0].VFAddress)
}
