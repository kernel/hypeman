package devices

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetVFHealthStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vf-health.json")
	require.NoError(t, InitVFHealth(path, defaultVFQuarantineThreshold))
	t.Cleanup(func() {
		vfHealth.mu.Lock()
		defer vfHealth.mu.Unlock()
		vfHealth.path = ""
		vfHealth.records = make(map[string]vfHealthRecord)
		vfHealth.threshold = defaultVFQuarantineThreshold
		vfHealth.loadErr = nil
		vfHealth.persistErr = nil
		vfHealth.syncDirFunc = syncDir
	})
	return path
}

// setVFHealthThreshold changes the quarantine threshold on the loaded store
// and re-evaluates recorded tallies, as a restart with a new
// gpu.vf_quarantine_threshold would.
func setVFHealthThreshold(n int) error {
	vfHealth.mu.Lock()
	defer vfHealth.mu.Unlock()
	vfHealth.threshold = n
	return vfHealth.requarantineLocked()
}

func vfHealthStoreUnavailable() bool {
	vfHealth.mu.Lock()
	defer vfHealth.mu.Unlock()
	return vfHealth.loadErr != nil || vfHealth.persistErr != nil
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
	require.NoError(t, setVFHealthThreshold(1))
	result, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: address, InstanceID: "quarantine-helper"})
	require.NoError(t, err)
	require.Equal(t, VFReportQuarantined, result.Outcome)
	require.NoError(t, setVFHealthThreshold(defaultVFQuarantineThreshold))
}

func TestVGPUAvailability(t *testing.T) {
	resetVFHealthStore(t)
	quarantineVF(t, "0000:82:00.4")
	result, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:82:00.6", InstanceID: "instance-1"})
	require.NoError(t, err)
	require.Equal(t, VFReportRecorded, result.Outcome)
	vfs := []VirtualFunction{
		{PCIAddress: "0000:82:00.4"},
		{PCIAddress: "0000:82:00.5", Allocated: true},
		{PCIAddress: "0000:82:00.6"},
	}

	availability, err := GetVGPUAvailability(VGPUFrameworkVendorVFIO, vfs)
	require.NoError(t, err)
	assert.Equal(t, 1, availability.AllocatableSlots, "a below-threshold failure tally must not remove the VF from placement")
	assert.Equal(t, 1, availability.QuarantinedSlots)

	availability, err = GetVGPUAvailability(VGPUFrameworkMdev, vfs)
	require.NoError(t, err)
	assert.Equal(t, 2, availability.AllocatableSlots)
	assert.Zero(t, availability.QuarantinedSlots)
}

func TestVGPUAvailabilityFailsWhenStoreUnavailable(t *testing.T) {
	path := resetVFHealthStore(t)
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0o644))
	require.Error(t, InitVFHealth(path, defaultVFQuarantineThreshold))

	_, err := GetVGPUAvailability(VGPUFrameworkVendorVFIO, []VirtualFunction{{PCIAddress: "0000:82:00.4"}})
	require.ErrorContains(t, err, "VF health state unavailable")

	availability, err := GetVGPUAvailability(VGPUFrameworkMdev, []VirtualFunction{{PCIAddress: "0000:82:00.4"}})
	require.NoError(t, err)
	assert.Equal(t, 1, availability.AllocatableSlots)
	assert.Zero(t, availability.QuarantinedSlots)

	restored := `{"version":1,"records":[{"vf_address":"0000:82:00.4","quarantined_at":"2026-08-20T00:00:00Z"}]}`
	require.NoError(t, os.WriteFile(path, []byte(restored), 0o644))
	availability, err = GetVGPUAvailability(VGPUFrameworkVendorVFIO, []VirtualFunction{{PCIAddress: "0000:82:00.4"}})
	require.NoError(t, err, "a repaired state file must re-enable placement without a new report")
	assert.Zero(t, availability.AllocatableSlots)
	assert.Equal(t, 1, availability.QuarantinedSlots)
}

func TestVGPUAvailabilityFailsClosedAfterPersistFailure(t *testing.T) {
	resetVFHealthStore(t)
	require.NoError(t, setVFHealthThreshold(1))
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, nil, 0o644))
	goodPath := vfHealth.path
	vfHealth.path = filepath.Join(blocker, "vf-health.json")

	_, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: "instance-1"})
	require.Error(t, err)
	assert.True(t, vfHealthStoreUnavailable())
	_, err = GetVGPUAvailability(VGPUFrameworkVendorVFIO, []VirtualFunction{{PCIAddress: "0000:e3:00.4"}})
	require.ErrorContains(t, err, "last write failed")

	vfHealth.path = goodPath
	require.NoError(t, RepairVFHealthStore())
	result, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: "instance-1"})
	require.NoError(t, err)
	assert.Equal(t, VFReportQuarantined, result.Outcome)
	assert.False(t, vfHealthStoreUnavailable())

	availability, err := GetVGPUAvailability(VGPUFrameworkVendorVFIO, []VirtualFunction{{PCIAddress: "0000:e3:00.4"}})
	require.NoError(t, err)
	assert.Zero(t, availability.AllocatableSlots)
	assert.Equal(t, 1, availability.QuarantinedSlots)
}

func TestCheckedAddressesRetriesFailedPersist(t *testing.T) {
	path := resetVFHealthStore(t)
	require.NoError(t, setVFHealthThreshold(1))
	_, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: "instance-1"})
	require.NoError(t, err)

	vfHealth.syncDirFunc = func(string) error { return errors.New("injected sync failure") }
	_, err = ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.5", InstanceID: "instance-2"})
	require.Error(t, err)
	assert.True(t, vfHealthStoreUnavailable())
	_, err = GetVGPUAvailability(VGPUFrameworkVendorVFIO, nil)
	require.ErrorContains(t, err, "last write failed")

	// No report arrives while placement is closed; a read alone must clear the latch.
	vfHealth.syncDirFunc = syncDir
	availability, err := GetVGPUAvailability(VGPUFrameworkVendorVFIO, []VirtualFunction{{PCIAddress: "0000:e3:00.4"}, {PCIAddress: "0000:e3:00.5"}})
	require.NoError(t, err)
	assert.False(t, vfHealthStoreUnavailable())
	assert.Equal(t, 1, availability.AllocatableSlots)
	assert.Equal(t, 1, availability.QuarantinedSlots)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var state vfHealthFile
	require.NoError(t, json.Unmarshal(data, &state))
	require.Len(t, state.Records, 1, "the retried write must persist the rolled-back in-memory state")
	assert.Equal(t, "0000:e3:00.4", state.Records[0].VFAddress)
}

func TestSetThresholdReevaluatesRecordedFailures(t *testing.T) {
	path := resetVFHealthStore(t)
	require.NoError(t, setVFHealthThreshold(3))
	for _, instance := range []string{"instance-1", "instance-2"} {
		result, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: instance})
		require.NoError(t, err)
		require.Equal(t, VFReportRecorded, result.Outcome)
	}

	require.NoError(t, setVFHealthThreshold(2))

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
	require.NoError(t, setVFHealthThreshold(3))
	for _, instance := range []string{"instance-1", "instance-2"} {
		result, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: instance})
		require.NoError(t, err)
		require.Equal(t, VFReportRecorded, result.Outcome)
	}

	// Simulate a restart with a lower configured threshold.
	require.NoError(t, InitVFHealth(path, 2))

	records := quarantinedVFs()
	require.Len(t, records, 1)
	assert.Equal(t, "0000:e3:00.4", records[0].VFAddress)
}

func TestReportVFInitFailureQuarantinesAtThreshold(t *testing.T) {
	path := resetVFHealthStore(t)

	result, err := ReportVFInitFailure(VFInitFailureReport{
		VFAddress:  "0000:e3:00.4",
		InstanceID: "instance-1",
		AssignedAt: FormatVFAssignedAt(time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)),
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
	assert.Equal(t, "0000:e3:00.4", records[0].VFAddress)
	require.NotNil(t, records[0].QuarantinedAt)
	require.Len(t, records[0].Failures, 2)
	assert.Equal(t, "instance-1", records[0].Failures[0].InstanceID)

	result, err = ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: "instance-3"})
	require.NoError(t, err)
	assert.Equal(t, VFReportUnchanged, result.Outcome)

	require.NoError(t, InitVFHealth(path, defaultVFQuarantineThreshold))
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

	require.NoError(t, InitVFHealth(path, defaultVFQuarantineThreshold))
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

func TestReportVFInitSuccessForOlderAssignmentKeepsQuarantine(t *testing.T) {
	path := resetVFHealthStore(t)
	vf := "0000:e3:00.4"
	older := VFInitFailureReport{
		VFAddress:  vf,
		InstanceID: "instance-1",
		AssignedAt: "2026-08-20T14:00:00Z",
	}
	_, err := ReportVFInitFailure(older)
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
		VFAddress:  older.VFAddress,
		InstanceID: older.InstanceID,
		AssignedAt: older.AssignedAt,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, success.Cleared)
	assert.False(t, success.Rescinded)

	require.NoError(t, InitVFHealth(path, defaultVFQuarantineThreshold))
	quarantined := quarantinedVFs()
	require.Len(t, quarantined, 1)
	require.Len(t, quarantined[0].Failures, 1)
	assert.Equal(t, trigger.InstanceID, quarantined[0].Failures[0].InstanceID)

	success, err = ReportVFInitSuccess(VFInitSuccessReport{
		VFAddress:  trigger.VFAddress,
		InstanceID: trigger.InstanceID,
		AssignedAt: trigger.AssignedAt,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, success.Cleared)
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

func TestReportVFInitFailureNoopDoesNotErrorWhilePersistFailed(t *testing.T) {
	resetVFHealthStore(t)
	report := VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: "instance-1"}
	_, err := ReportVFInitFailure(report)
	require.NoError(t, err)

	vfHealth.mu.Lock()
	vfHealth.persistErr = errors.New("injected persist failure")
	vfHealth.mu.Unlock()

	result, err := ReportVFInitFailure(report)
	require.NoError(t, err)
	assert.Equal(t, VFReportUnchanged, result.Outcome, "an already-recorded assignment has nothing to write")

	_, err = ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: "instance-2"})
	require.ErrorContains(t, err, "last write failed")
}

func TestReportVFInitSuccessNoopDoesNotRetryFailedPersist(t *testing.T) {
	resetVFHealthStore(t)
	var syncCalls int
	vfHealth.mu.Lock()
	vfHealth.persistErr = errors.New("injected persist failure")
	vfHealth.syncDirFunc = func(string) error {
		syncCalls++
		return nil
	}
	vfHealth.mu.Unlock()

	result, err := ReportVFInitSuccess(VFInitSuccessReport{
		VFAddress:  "0000:e3:00.4",
		InstanceID: "healthy-instance",
	})
	require.NoError(t, err)
	assert.Equal(t, VFSuccessResult{}, result)
	assert.Zero(t, syncCalls)
	assert.True(t, VFHealthStoreUnavailable())

	require.NoError(t, RepairVFHealthStore())
	assert.Equal(t, 2, syncCalls, "one repair performs one parent and state-directory sync")
	assert.False(t, VFHealthStoreUnavailable())
}

// withVendorVFIOMuHeld fails the test if fn blocks on vendorVFIOMu.
func withVendorVFIOMuHeld(t *testing.T, fn func()) {
	t.Helper()
	vendorVFIOMu.Lock()
	defer vendorVFIOMu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("call blocked on vendorVFIOMu")
	}
}

func TestReportVFInitSuccessForHealthyVFSkipsVendorVFIOMu(t *testing.T) {
	resetVFHealthStore(t)
	withVendorVFIOMuHeld(t, func() {
		result, err := ReportVFInitSuccess(VFInitSuccessReport{VFAddress: "0000:e3:00.4", InstanceID: "healthy-instance"})
		assert.NoError(t, err)
		assert.Equal(t, VFSuccessResult{}, result)
	})

	_, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: "instance-1"})
	require.NoError(t, err)
	result, err := ReportVFInitSuccess(VFInitSuccessReport{VFAddress: "0000:e3:00.4", InstanceID: "instance-1"})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Cleared, "a VF with tallied failures still takes the full path")
}

func TestReportVFInitSuccessInvalidAddressStillErrors(t *testing.T) {
	resetVFHealthStore(t)
	_, err := ReportVFInitSuccess(VFInitSuccessReport{VFAddress: "not-a-vf", InstanceID: "instance-1"})
	require.ErrorContains(t, err, "invalid VF address")
}

func TestRepairVFHealthStoreHealthySkipsVendorVFIOMu(t *testing.T) {
	resetVFHealthStore(t)
	var syncCalls int
	vfHealth.mu.Lock()
	vfHealth.syncDirFunc = func(string) error {
		syncCalls++
		return nil
	}
	vfHealth.mu.Unlock()

	withVendorVFIOMuHeld(t, func() {
		assert.NoError(t, RepairVFHealthStore())
	})
	assert.Zero(t, syncCalls, "a healthy store must not be rewritten")
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

func TestReportVFInitFailureRetriesParentSyncAfterFailure(t *testing.T) {
	resetVFHealthStore(t)
	parentDir := t.TempDir()
	vfHealth.path = filepath.Join(parentDir, "gpu", "vf-health.json")

	parentSyncs := 0
	retrySawPersistErr := false
	vfHealth.syncDirFunc = func(path string) error {
		if path != parentDir {
			return syncDir(path)
		}
		parentSyncs++
		if parentSyncs == 1 {
			return errors.New("injected parent sync failure")
		}
		if parentSyncs == 2 {
			retrySawPersistErr = vfHealth.persistErr != nil
		}
		return syncDir(path)
	}

	report := VFInitFailureReport{VFAddress: "0000:e3:00.4", InstanceID: "instance-1"}
	_, err := ReportVFInitFailure(report)
	require.ErrorContains(t, err, "sync VF health state parent dir")
	assert.True(t, vfHealthStoreUnavailable())

	require.NoError(t, RepairVFHealthStore())
	result, err := ReportVFInitFailure(report)
	require.NoError(t, err)
	assert.Equal(t, VFReportRecorded, result.Outcome)
	assert.Equal(t, 3, parentSyncs)
	assert.True(t, retrySawPersistErr, "retry must sync the parent before clearing the write failure")
	assert.False(t, vfHealthStoreUnavailable())
}

func TestReportVFInitFailureRetainsRenamedStateAfterSyncFailure(t *testing.T) {
	path := resetVFHealthStore(t)
	vf := "0000:e3:00.4"
	_, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: vf, InstanceID: "instance-1"})
	require.NoError(t, err)

	vfHealth.syncDirFunc = func(path string) error {
		if path == filepath.Dir(vfHealth.path) {
			return errors.New("injected sync failure")
		}
		return syncDir(path)
	}
	_, err = ReportVFInitFailure(VFInitFailureReport{VFAddress: vf, InstanceID: "instance-2"})
	require.ErrorContains(t, err, "sync VF health state dir")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var state vfHealthFile
	require.NoError(t, json.Unmarshal(data, &state))
	require.Len(t, state.Records, 1)
	assert.NotNil(t, state.Records[0].QuarantinedAt)
	require.Len(t, quarantinedVFs(), 1, "memory must retain state already renamed into place")
	assert.True(t, vfHealthStoreUnavailable())

	vfHealth.syncDirFunc = syncDir
	require.NoError(t, RepairVFHealthStore())
	result, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: "0000:e3:00.5", InstanceID: "other-instance"})
	require.NoError(t, err)
	assert.Equal(t, VFReportRecorded, result.Outcome)
	assert.False(t, vfHealthStoreUnavailable())

	data, err = os.ReadFile(path)
	require.NoError(t, err)
	state = vfHealthFile{}
	require.NoError(t, json.Unmarshal(data, &state))
	found := false
	for _, record := range state.Records {
		if record.VFAddress == vf {
			found = true
			assert.NotNil(t, record.QuarantinedAt, "a later write must not erase the renamed quarantine")
		}
	}
	require.True(t, found)

	availability, err := GetVGPUAvailability(VGPUFrameworkVendorVFIO, []VirtualFunction{{PCIAddress: vf}})
	require.NoError(t, err)
	assert.Zero(t, availability.AllocatableSlots)
	assert.Equal(t, 1, availability.QuarantinedSlots)
}

func TestReportRetriesFailedThresholdPersistence(t *testing.T) {
	path := resetVFHealthStore(t)
	vf := "0000:e3:00.4"
	require.NoError(t, setVFHealthThreshold(3))
	for _, instance := range []string{"instance-1", "instance-2"} {
		_, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: vf, InstanceID: instance})
		require.NoError(t, err)
	}

	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, nil, 0644))
	vfHealth.path = filepath.Join(blocker, "vf-health.json")
	require.Error(t, setVFHealthThreshold(2))
	assert.True(t, vfHealthStoreUnavailable())

	vfHealth.path = path
	require.NoError(t, RepairVFHealthStore())
	result, err := ReportVFInitFailure(VFInitFailureReport{VFAddress: vf, InstanceID: "instance-3"})
	require.NoError(t, err)
	assert.Equal(t, VFReportUnchanged, result.Outcome)
	assert.False(t, vfHealthStoreUnavailable())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var state vfHealthFile
	require.NoError(t, json.Unmarshal(data, &state))
	require.Len(t, state.Records, 1)
	assert.NotNil(t, state.Records[0].QuarantinedAt)
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

func TestRepairVFHealthStoreRecoversPostRenameSuccessClearFailure(t *testing.T) {
	path := resetVFHealthStore(t)
	report := VFInitFailureReport{
		VFAddress:  "0000:e3:00.4",
		InstanceID: "instance-1",
		AssignedAt: "2026-08-20T15:00:00Z",
	}
	_, err := ReportVFInitFailure(report)
	require.NoError(t, err)

	failed := false
	vfHealth.syncDirFunc = func(path string) error {
		if path == filepath.Dir(vfHealth.path) && !failed {
			failed = true
			return errors.New("injected sync failure")
		}
		return syncDir(path)
	}
	_, err = ReportVFInitSuccess(VFInitSuccessReport{
		VFAddress:  report.VFAddress,
		InstanceID: report.InstanceID,
		AssignedAt: report.AssignedAt,
	})
	require.ErrorContains(t, err, "sync VF health state dir")
	assert.True(t, VFHealthStoreUnavailable())

	vfHealth.mu.Lock()
	_, exists := vfHealth.records[report.VFAddress]
	vfHealth.mu.Unlock()
	assert.False(t, exists, "memory must retain the clear renamed into place")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var state vfHealthFile
	require.NoError(t, json.Unmarshal(data, &state))
	assert.Empty(t, state.Records)

	require.NoError(t, RepairVFHealthStore())
	assert.False(t, VFHealthStoreUnavailable())
	_, err = GetVGPUAvailability(VGPUFrameworkVendorVFIO, []VirtualFunction{{PCIAddress: report.VFAddress}})
	require.NoError(t, err)
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
			wantErr: `duplicate failure for instance "instance-1" assigned at "a"`,
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
			require.ErrorContains(t, InitVFHealth(path, defaultVFQuarantineThreshold), tt.wantErr)
			assert.True(t, vfHealthStoreUnavailable())
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
	require.Error(t, InitVFHealth(path, defaultVFQuarantineThreshold))

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

func TestInitVFHealthFailsWhenReevaluatedQuarantineCannotPersist(t *testing.T) {
	path := resetVFHealthStore(t)
	// Two persisted tallies meet the default threshold, so loading them
	// quarantines the VF and must write that back.
	state := `{"version":1,"records":[{"vf_address":"0000:e3:00.4","failures":[` +
		`{"instance_id":"instance-1","reported_at":"2026-08-20T00:00:00Z"},` +
		`{"instance_id":"instance-2","reported_at":"2026-08-20T01:00:00Z"}]}]}`
	require.NoError(t, os.WriteFile(path, []byte(state), 0o644))
	vfHealth.syncDirFunc = func(string) error { return errors.New("injected sync failure") }

	err := InitVFHealth(path, defaultVFQuarantineThreshold)
	require.ErrorContains(t, err, "injected sync failure")
	require.Len(t, quarantinedVFs(), 1, "the re-evaluated quarantine must stay in effect in memory")
	assert.True(t, vfHealthStoreUnavailable())
	_, err = GetVGPUAvailability(VGPUFrameworkVendorVFIO, []VirtualFunction{{PCIAddress: "0000:e3:00.4"}})
	require.ErrorContains(t, err, "last write failed")

	vfHealth.syncDirFunc = syncDir
	availability, err := GetVGPUAvailability(VGPUFrameworkVendorVFIO, []VirtualFunction{{PCIAddress: "0000:e3:00.4"}})
	require.NoError(t, err, "a read must retry the failed write once the disk recovers")
	assert.False(t, vfHealthStoreUnavailable())
	assert.Zero(t, availability.AllocatableSlots)
	assert.Equal(t, 1, availability.QuarantinedSlots)
}
