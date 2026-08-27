package devices

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetVFHealthStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vf-health.json")
	require.NoError(t, initVFHealth(path))
	t.Cleanup(func() {
		vfHealth.mu.Lock()
		defer vfHealth.mu.Unlock()
		vfHealth.path = ""
		vfHealth.records = make(map[string]vfHealthRecord)
		vfHealth.threshold = defaultVFQuarantineThreshold
		vfHealth.loadErr = nil
		vfHealth.persistErr = nil
	})
	return path
}

func quarantinedVFs() []vfHealthRecord {
	vfHealth.mu.Lock()
	defer vfHealth.mu.Unlock()
	records := vfHealth.sortedRecordsLocked()
	result := records[:0]
	for _, record := range records {
		if record.QuarantinedAt != nil {
			result = append(result, record)
		}
	}
	return result
}

func quarantineVF(t *testing.T, address string) {
	t.Helper()
	SetVFQuarantineThreshold(1)
	result, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: address, InstanceID: "quarantine-helper"})
	require.NoError(t, err)
	require.Equal(t, VFReportQuarantined, result.Outcome)
	SetVFQuarantineThreshold(defaultVFQuarantineThreshold)
}

func TestVGPUAvailability(t *testing.T) {
	resetVFHealthStore(t)
	quarantineVF(t, "0000:82:00.4")
	vfs := []VirtualFunction{
		{PCIAddress: "0000:82:00.4"},
		{PCIAddress: "0000:82:00.5", Allocated: true},
		{PCIAddress: "0000:82:00.6"},
	}

	available, quarantined, err := VGPUAvailability(VGPUFrameworkVendorVFIO, vfs)
	require.NoError(t, err)
	assert.Equal(t, 1, available)
	assert.Equal(t, 1, quarantined)

	available, quarantined, err = VGPUAvailability(VGPUFrameworkMdev, vfs)
	require.NoError(t, err)
	assert.Equal(t, 2, available)
	assert.Zero(t, quarantined)
}

func TestVGPUAvailabilityExcludesOnlyQuarantinedVFs(t *testing.T) {
	resetVFHealthStore(t)
	result, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:82:00.4", InstanceID: "instance-1"})
	require.NoError(t, err)
	require.Equal(t, VFReportRecorded, result.Outcome)

	available, quarantined, err := VGPUAvailability(VGPUFrameworkVendorVFIO, []VirtualFunction{{PCIAddress: "0000:82:00.4"}})
	require.NoError(t, err)
	assert.Equal(t, 1, available, "a below-threshold failure tally must not remove the VF from placement")
	assert.Zero(t, quarantined)
}

func TestVGPUAvailabilityFailsWhenStoreUnavailable(t *testing.T) {
	path := resetVFHealthStore(t)
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0o644))
	require.Error(t, initVFHealth(path))

	_, _, err := VGPUAvailability(VGPUFrameworkVendorVFIO, []VirtualFunction{{PCIAddress: "0000:82:00.4"}})
	require.ErrorContains(t, err, "VF health state unavailable")

	available, quarantined, err := VGPUAvailability(VGPUFrameworkMdev, []VirtualFunction{{PCIAddress: "0000:82:00.4"}})
	require.NoError(t, err)
	assert.Equal(t, 1, available)
	assert.Zero(t, quarantined)
}

func TestVGPUAvailabilityFailsClosedAfterPersistFailure(t *testing.T) {
	resetVFHealthStore(t)
	SetVFQuarantineThreshold(1)
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, nil, 0o644))
	goodPath := vfHealth.path
	vfHealth.path = filepath.Join(blocker, "vf-health.json")

	_, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: "instance-1"})
	require.Error(t, err)
	assert.True(t, VFHealthStoreUnavailable())
	_, _, err = VGPUAvailability(VGPUFrameworkVendorVFIO, []VirtualFunction{{PCIAddress: "0000:e3:00.4"}})
	require.ErrorContains(t, err, "last write failed")

	vfHealth.path = goodPath
	result, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: "instance-1"})
	require.NoError(t, err)
	assert.Equal(t, VFReportQuarantined, result.Outcome)
	assert.False(t, VFHealthStoreUnavailable())

	available, quarantined, err := VGPUAvailability(VGPUFrameworkVendorVFIO, []VirtualFunction{{PCIAddress: "0000:e3:00.4"}})
	require.NoError(t, err)
	assert.Zero(t, available)
	assert.Equal(t, 1, quarantined)
}

func TestSetVFQuarantineThresholdReevaluatesRecordedFailures(t *testing.T) {
	path := resetVFHealthStore(t)
	SetVFQuarantineThreshold(3)
	for _, instance := range []string{"instance-1", "instance-2"} {
		result, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: instance})
		require.NoError(t, err)
		require.Equal(t, VFReportRecorded, result.Outcome)
	}

	SetVFQuarantineThreshold(2)

	records := quarantinedVFs()
	require.Len(t, records, 1)
	assert.Equal(t, "0000:e3:00.4", records[0].VFAddress)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var state vfHealthFile
	require.NoError(t, json.Unmarshal(data, &state))
	require.Len(t, state.Records, 1)
	assert.NotNil(t, state.Records[0].QuarantinedAt, "the re-evaluated quarantine must be persisted")
}

func TestLoadReevaluatesTalliesAgainstConfiguredThreshold(t *testing.T) {
	path := resetVFHealthStore(t)
	SetVFQuarantineThreshold(3)
	for _, instance := range []string{"instance-1", "instance-2"} {
		result, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: instance})
		require.NoError(t, err)
		require.Equal(t, VFReportRecorded, result.Outcome)
	}

	// Simulate a restart where the threshold is configured lower before the
	// persisted tallies are loaded.
	vfHealth.mu.Lock()
	vfHealth.records = make(map[string]vfHealthRecord)
	vfHealth.threshold = 2
	vfHealth.mu.Unlock()
	require.NoError(t, initVFHealth(path))

	records := quarantinedVFs()
	require.Len(t, records, 1)
	assert.Equal(t, "0000:e3:00.4", records[0].VFAddress)
}

func TestReportVFInitFailureQuarantinesAtThreshold(t *testing.T) {
	path := resetVFHealthStore(t)

	result, err := ReportVFInitFailure(VFInitFailureReport{
		VFAddress:  "0000:e3:00.4",
		InstanceID: "instance-1",
		AssignedAt: "2026-08-20T15:00:00Z",
	})
	require.NoError(t, err)
	assert.Equal(t, VFReportRecorded, result.Outcome)
	assert.Equal(t, 1, result.Failures)
	assert.Equal(t, defaultVFQuarantineThreshold, result.Threshold)
	assert.Empty(t, quarantinedVFs(), "one failure must not quarantine at the default threshold")

	result, err = ReportVFInitFailure(VFInitFailureReport{
		VFAddress:  "0000:e3:00.4",
		InstanceID: "instance-2",
		AssignedAt: "2026-08-20T16:00:00Z",
	})
	require.NoError(t, err)
	assert.Equal(t, VFReportQuarantined, result.Outcome)
	assert.Equal(t, 2, result.Failures)

	records := quarantinedVFs()
	require.Len(t, records, 1)
	assert.Equal(t, 1, TotalQuarantinedVFs())
	assert.Equal(t, "0000:e3:00.4", records[0].VFAddress)
	require.NotNil(t, records[0].QuarantinedAt)
	require.Len(t, records[0].Failures, 2)
	assert.Equal(t, "instance-1", records[0].Failures[0].InstanceID)

	result, err = ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: "instance-3"})
	require.NoError(t, err)
	assert.Equal(t, VFReportUnchanged, result.Outcome)

	require.NoError(t, initVFHealth(path))
	reloaded := quarantinedVFs()
	require.Len(t, reloaded, 1)
	assert.Equal(t, "0000:e3:00.4", reloaded[0].VFAddress)
	require.Len(t, reloaded[0].Failures, 2)
}

func TestReportVFInitFailureDeduplicatesAssignments(t *testing.T) {
	resetVFHealthStore(t)

	report := VFInitFailureReport{
		VFAddress:  "0000:e3:00.4",
		InstanceID: "instance-1",
		AssignedAt: "2026-08-20T15:00:00Z",
	}
	result, err := ReportVFInitFailure(report)
	require.NoError(t, err)
	assert.Equal(t, VFReportRecorded, result.Outcome)

	result, err = ReportVFInitFailure(report)
	require.NoError(t, err)
	assert.Equal(t, VFReportUnchanged, result.Outcome)
	assert.Equal(t, 1, result.Failures)
	assert.Empty(t, quarantinedVFs(), "a rescanned assignment must not count toward the threshold twice")
}

func TestReportVFInitFailureRespectsConfiguredThreshold(t *testing.T) {
	resetVFHealthStore(t)
	SetVFQuarantineThreshold(3)

	for i, instance := range []string{"instance-1", "instance-2"} {
		result, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: instance})
		require.NoError(t, err)
		assert.Equal(t, VFReportRecorded, result.Outcome)
		assert.Equal(t, i+1, result.Failures)
		assert.Equal(t, 3, result.Threshold)
	}

	result, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: "instance-3"})
	require.NoError(t, err)
	assert.Equal(t, VFReportQuarantined, result.Outcome)
}

func TestReportVFInitSuccessClearsFailureTally(t *testing.T) {
	path := resetVFHealthStore(t)
	report := VFInitFailureReport{
		VFAddress:  "0000:e3:00.4",
		InstanceID: "instance-1",
		AssignedAt: "2026-08-20T15:00:00Z",
	}

	_, err := ReportVFInitFailure(report)
	require.NoError(t, err)

	success := VFInitSuccessReport{
		VFAddress:  report.VFAddress,
		InstanceID: report.InstanceID,
		AssignedAt: report.AssignedAt,
	}
	successResult, err := ReportVFInitSuccess(success)
	require.NoError(t, err)
	assert.Equal(t, 1, successResult.Cleared)
	assert.False(t, successResult.Rescinded)

	successResult, err = ReportVFInitSuccess(success)
	require.NoError(t, err)
	assert.Zero(t, successResult.Cleared)

	require.NoError(t, initVFHealth(path))
	result, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: report.VFAddress, InstanceID: "instance-3"})
	require.NoError(t, err)
	assert.Equal(t, VFReportRecorded, result.Outcome)
	assert.Equal(t, 1, result.Failures)
}

func TestReportVFInitSuccessRescindsQuarantineTriggeredByAssignment(t *testing.T) {
	resetVFHealthStore(t)
	vf := "0000:e3:00.4"
	_, err := ReportVFInitFailure(VFInitFailureReport{
		VFAddress:  vf,
		InstanceID: "instance-1",
		AssignedAt: "2026-08-20T14:00:00Z",
	})
	require.NoError(t, err)
	trigger := VFInitFailureReport{
		VFAddress:  vf,
		InstanceID: "instance-2",
		AssignedAt: "2026-08-20T15:00:00Z",
	}
	result, err := ReportVFInitFailure(trigger)
	require.NoError(t, err)
	require.Equal(t, VFReportQuarantined, result.Outcome)

	success, err := ReportVFInitSuccess(VFInitSuccessReport{
		VFAddress:  trigger.VFAddress,
		InstanceID: trigger.InstanceID,
		AssignedAt: trigger.AssignedAt,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, success.Cleared)
	assert.True(t, success.Rescinded)
	assert.Empty(t, quarantinedVFs())
}

func TestReportVFInitSuccessWithoutMatchingFailureClearsNothing(t *testing.T) {
	resetVFHealthStore(t)
	_, err := ReportVFInitFailure(VFInitFailureReport{
		VFAddress:  "0000:e3:00.4",
		InstanceID: "instance-1",
		AssignedAt: "2026-08-20T15:00:00Z",
	})
	require.NoError(t, err)

	result, err := ReportVFInitSuccess(VFInitSuccessReport{
		VFAddress:  "0000:e3:00.4",
		InstanceID: "instance-2",
		AssignedAt: "2026-08-20T16:00:00Z",
	})
	require.NoError(t, err)
	assert.Zero(t, result.Cleared)
	assert.False(t, result.Rescinded)
}

func TestReportVFInitSuccessNeverClearsAnotherAssignmentsQuarantine(t *testing.T) {
	resetVFHealthStore(t)
	quarantineVF(t, "0000:e3:00.4")

	result, err := ReportVFInitSuccess(VFInitSuccessReport{
		VFAddress:  "0000:e3:00.4",
		InstanceID: "another-instance",
	})
	require.NoError(t, err)
	assert.Zero(t, result.Cleared)
	assert.False(t, result.Rescinded)
	require.Len(t, quarantinedVFs(), 1)
}

func TestReportVFInitFailureRejectsInvalidAddress(t *testing.T) {
	resetVFHealthStore(t)

	_, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "not-a-pci-address"})
	require.ErrorContains(t, err, "invalid VF address")
	_, err = ReportVFInitSuccess(VFInitSuccessReport{VFAddress: "not-a-pci-address"})
	require.ErrorContains(t, err, "invalid VF address")
	assert.Empty(t, quarantinedVFs())
}

func TestReportVFInitFailureRollsBackOnPersistFailure(t *testing.T) {
	resetVFHealthStore(t)
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, nil, 0644))
	vfHealth.path = filepath.Join(blocker, "vf-health.json")

	_, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: "instance-1"})
	require.Error(t, err)

	vfHealth.mu.Lock()
	_, exists := vfHealth.records["0000:e3:00.4"]
	vfHealth.mu.Unlock()
	assert.False(t, exists, "a failure whose persist failed must be retried by the next report")
}

func TestReportVFInitSuccessRollsBackOnPersistFailure(t *testing.T) {
	resetVFHealthStore(t)
	_, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: "instance-1"})
	require.NoError(t, err)

	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, nil, 0644))
	goodPath := vfHealth.path
	vfHealth.path = filepath.Join(blocker, "vf-health.json")

	_, err = ReportVFInitSuccess(VFInitSuccessReport{VFAddress: "0000:e3:00.4", InstanceID: "instance-1"})
	require.Error(t, err)
	vfHealth.path = goodPath

	vfHealth.mu.Lock()
	record, exists := vfHealth.records["0000:e3:00.4"]
	vfHealth.mu.Unlock()
	require.True(t, exists, "a clear whose persist failed must be restored in memory")
	assert.Len(t, record.Failures, 1)
}

func TestCheckedAddressesFailsClosedOnUnloadedState(t *testing.T) {
	path := resetVFHealthStore(t)
	quarantineVF(t, "0000:e3:00.4")

	require.NoError(t, os.WriteFile(path, []byte("not json"), 0644))
	require.Error(t, initVFHealth(path))

	_, err := vfHealth.checkedAddresses()
	require.Error(t, err)

	restored := `{"version":1,"records":[{"vf_address":"0000:e3:00.4","quarantined_at":"2026-08-20T00:00:00Z"}]}`
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
			name:    "unsupported version",
			state:   `{"version":2,"records":[]}`,
			wantErr: "unsupported version 2",
		},
		{
			name:    "missing records",
			state:   `{"version":1}`,
			wantErr: "expected a records array",
		},
		{
			name:    "invalid address",
			state:   `{"version":1,"records":[{"vf_address":"not-a-pci-address","quarantined_at":"2026-08-20T00:00:00Z"}]}`,
			wantErr: "invalid VF address",
		},
		{
			name:    "neither quarantined nor failed",
			state:   `{"version":1,"records":[{"vf_address":"0000:e3:00.4"}]}`,
			wantErr: "neither quarantined nor any recorded failures",
		},
		{
			name:    "failure missing report timestamp",
			state:   `{"version":1,"records":[{"vf_address":"0000:e3:00.4","failures":[{"instance_id":"instance-1"}]}]}`,
			wantErr: "missing report timestamp",
		},
		{
			name:    "duplicate assignment",
			state:   `{"version":1,"records":[{"vf_address":"0000:e3:00.4","failures":[{"instance_id":"instance-1","assigned_at":"a","reported_at":"2026-08-20T00:00:00Z"},{"instance_id":"instance-1","assigned_at":"a","reported_at":"2026-08-21T00:00:00Z"}]}]}`,
			wantErr: "duplicate failure for assignment",
		},
		{
			name:    "duplicate address",
			state:   `{"version":1,"records":[{"vf_address":"0000:e3:00.4","quarantined_at":"2026-08-20T00:00:00Z"},{"vf_address":"0000:e3:00.4","quarantined_at":"2026-08-21T00:00:00Z"}]}`,
			wantErr: "duplicate VF address",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := resetVFHealthStore(t)
			require.NoError(t, os.WriteFile(path, []byte(tt.state), 0644))
			require.ErrorContains(t, initVFHealth(path), tt.wantErr)
			assert.True(t, VFHealthStoreUnavailable())
			assert.Empty(t, quarantinedVFs())

			_, err := vfHealth.checkedAddresses()
			require.Error(t, err)
		})
	}
}

func TestReportVFInitFailureRefusesToClobberUnloadedState(t *testing.T) {
	path := resetVFHealthStore(t)
	quarantineVF(t, "0000:e3:00.4")

	require.NoError(t, os.WriteFile(path, []byte("not json"), 0644))
	require.Error(t, initVFHealth(path))

	_, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.5"})
	require.Error(t, err)
	_, err = ReportVFInitSuccess(VFInitSuccessReport{VFAddress: "0000:e3:00.5"})
	require.Error(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "not json", string(data), "a failed load must not be overwritten by later reports")

	restored := `{"version":1,"records":[{"vf_address":"0000:e3:00.4","quarantined_at":"2026-08-20T00:00:00Z"}]}`
	require.NoError(t, os.WriteFile(path, []byte(restored), 0644))
	quarantineVF(t, "0000:e3:00.5")
	records := quarantinedVFs()
	require.Len(t, records, 2, "reload must recover the previously persisted quarantine")
	assert.Equal(t, "0000:e3:00.4", records[0].VFAddress)
}
