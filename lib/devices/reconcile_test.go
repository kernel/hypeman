package devices

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockLivenessChecker implements InstanceLivenessChecker for testing
type mockLivenessChecker struct {
	runningInstances map[string]bool      // instanceID -> isRunning
	instanceDevices  map[string][]string  // instanceID -> deviceIDs
}

func newMockLivenessChecker() *mockLivenessChecker {
	return &mockLivenessChecker{
		runningInstances: make(map[string]bool),
		instanceDevices:  make(map[string][]string),
	}
}

func (m *mockLivenessChecker) IsInstanceRunning(ctx context.Context, instanceID string) bool {
	return m.runningInstances[instanceID]
}

func (m *mockLivenessChecker) GetInstanceDevices(ctx context.Context, instanceID string) []string {
	return m.instanceDevices[instanceID]
}

func (m *mockLivenessChecker) ListAllInstanceDevices(ctx context.Context) map[string][]string {
	return m.instanceDevices
}

func (m *mockLivenessChecker) DetectSuspiciousVMMProcesses(ctx context.Context) int {
	return 0 // Mock returns no suspicious processes
}

func (m *mockLivenessChecker) setRunning(instanceID string, running bool) {
	m.runningInstances[instanceID] = running
}

func (m *mockLivenessChecker) setInstanceDevices(instanceID string, deviceIDs []string) {
	m.instanceDevices[instanceID] = deviceIDs
}

// setupTestManager creates a manager with a temporary directory for testing
func setupTestManager(t *testing.T) (*manager, *paths.Paths, string) {
	t.Helper()
	tmpDir := t.TempDir()
	p := paths.New(tmpDir)
	
	// Create devices directory
	require.NoError(t, os.MkdirAll(p.DevicesDir(), 0755))
	
	mgr := &manager{
		paths:      p,
		vfioBinder: NewVFIOBinder(),
	}
	
	return mgr, p, tmpDir
}

// createTestDevice creates a device in the test directory
func createTestDevice(t *testing.T, p *paths.Paths, device *Device) {
	t.Helper()
	deviceDir := p.DeviceDir(device.Id)
	require.NoError(t, os.MkdirAll(deviceDir, 0755))
	
	data, err := json.MarshalIndent(device, "", "  ")
	require.NoError(t, err)
	
	require.NoError(t, os.WriteFile(p.DeviceMetadata(device.Id), data, 0644))
}

// createTestInstanceDir creates an instance directory (simulating instance existence)
func createTestInstanceDir(t *testing.T, p *paths.Paths, instanceID string) {
	t.Helper()
	instanceDir := p.InstanceDir(instanceID)
	require.NoError(t, os.MkdirAll(instanceDir, 0755))
}

func TestReconcileDevices_NoDevices(t *testing.T) {
	mgr, _, _ := setupTestManager(t)
	ctx := context.Background()
	
	err := mgr.ReconcileDevices(ctx)
	require.NoError(t, err)
}

func TestReconcileDevices_OrphanedAttachment_NoLivenessChecker(t *testing.T) {
	mgr, p, _ := setupTestManager(t)
	ctx := context.Background()
	
	instanceID := "orphaned-instance-123"
	deviceID := "device-abc"
	
	// Create device with AttachedTo pointing to non-existent instance
	device := &Device{
		Id:         deviceID,
		Name:       "test-gpu",
		Type:       DeviceTypeGPU,
		PCIAddress: "0000:99:00.0", // Non-existent for test
		VendorID:   "10de",
		DeviceID:   "1234",
		AttachedTo: &instanceID,
		CreatedAt:  time.Now(),
	}
	createTestDevice(t, p, device)
	
	// Don't create the instance directory - it's orphaned
	
	// Run reconciliation
	err := mgr.ReconcileDevices(ctx)
	require.NoError(t, err)
	
	// Verify attachment was cleared
	updatedDevice, err := mgr.loadDevice(deviceID)
	require.NoError(t, err)
	assert.Nil(t, updatedDevice.AttachedTo, "AttachedTo should be cleared for orphaned device")
}

func TestReconcileDevices_ValidAttachment_NoLivenessChecker(t *testing.T) {
	mgr, p, _ := setupTestManager(t)
	ctx := context.Background()
	
	instanceID := "valid-instance-123"
	deviceID := "device-abc"
	
	// Create device with AttachedTo pointing to existing instance
	device := &Device{
		Id:         deviceID,
		Name:       "test-gpu",
		Type:       DeviceTypeGPU,
		PCIAddress: "0000:99:00.0",
		VendorID:   "10de",
		DeviceID:   "1234",
		AttachedTo: &instanceID,
		CreatedAt:  time.Now(),
	}
	createTestDevice(t, p, device)
	
	// Create the instance directory - it exists
	createTestInstanceDir(t, p, instanceID)
	
	// Run reconciliation
	err := mgr.ReconcileDevices(ctx)
	require.NoError(t, err)
	
	// Verify attachment was NOT cleared (instance exists)
	updatedDevice, err := mgr.loadDevice(deviceID)
	require.NoError(t, err)
	require.NotNil(t, updatedDevice.AttachedTo, "AttachedTo should NOT be cleared for valid device")
	assert.Equal(t, instanceID, *updatedDevice.AttachedTo)
}

func TestReconcileDevices_OrphanedAttachment_WithLivenessChecker(t *testing.T) {
	mgr, p, _ := setupTestManager(t)
	ctx := context.Background()
	
	// Set up liveness checker
	liveness := newMockLivenessChecker()
	mgr.livenessChecker = liveness
	
	instanceID := "stopped-instance-123"
	deviceID := "device-abc"
	
	// Create device with AttachedTo
	device := &Device{
		Id:         deviceID,
		Name:       "test-gpu",
		Type:       DeviceTypeGPU,
		PCIAddress: "0000:99:00.0",
		VendorID:   "10de",
		DeviceID:   "1234",
		AttachedTo: &instanceID,
		CreatedAt:  time.Now(),
	}
	createTestDevice(t, p, device)
	
	// Create instance directory but mark as NOT running
	createTestInstanceDir(t, p, instanceID)
	liveness.setRunning(instanceID, false) // Stopped/standby
	
	// Run reconciliation
	err := mgr.ReconcileDevices(ctx)
	require.NoError(t, err)
	
	// Verify attachment was cleared (instance not running)
	updatedDevice, err := mgr.loadDevice(deviceID)
	require.NoError(t, err)
	assert.Nil(t, updatedDevice.AttachedTo, "AttachedTo should be cleared for non-running instance")
}

func TestReconcileDevices_ValidAttachment_WithLivenessChecker(t *testing.T) {
	mgr, p, _ := setupTestManager(t)
	ctx := context.Background()
	
	// Set up liveness checker
	liveness := newMockLivenessChecker()
	mgr.livenessChecker = liveness
	
	instanceID := "running-instance-123"
	deviceID := "device-abc"
	
	// Create device with AttachedTo
	device := &Device{
		Id:         deviceID,
		Name:       "test-gpu",
		Type:       DeviceTypeGPU,
		PCIAddress: "0000:99:00.0",
		VendorID:   "10de",
		DeviceID:   "1234",
		AttachedTo: &instanceID,
		CreatedAt:  time.Now(),
	}
	createTestDevice(t, p, device)
	
	// Create instance and mark as running
	createTestInstanceDir(t, p, instanceID)
	liveness.setRunning(instanceID, true) // Running
	
	// Run reconciliation
	err := mgr.ReconcileDevices(ctx)
	require.NoError(t, err)
	
	// Verify attachment was NOT cleared (instance is running)
	updatedDevice, err := mgr.loadDevice(deviceID)
	require.NoError(t, err)
	require.NotNil(t, updatedDevice.AttachedTo, "AttachedTo should NOT be cleared for running instance")
	assert.Equal(t, instanceID, *updatedDevice.AttachedTo)
}

func TestReconcileDevices_TwoWayMismatch_InstanceRefsUnknownDevice(t *testing.T) {
	mgr, _, _ := setupTestManager(t)
	ctx := context.Background()
	
	// Set up liveness checker with instance that references unknown device
	liveness := newMockLivenessChecker()
	mgr.livenessChecker = liveness
	
	instanceID := "instance-with-ghost-device"
	unknownDeviceID := "device-that-doesnt-exist"
	
	// Instance references a device that doesn't exist
	liveness.setInstanceDevices(instanceID, []string{unknownDeviceID})
	liveness.setRunning(instanceID, true)
	
	// Run reconciliation - should not error, just log the mismatch
	err := mgr.ReconcileDevices(ctx)
	require.NoError(t, err)
	// Note: We can't easily verify log output, but the test ensures no panic/error
}

func TestReconcileDevices_TwoWayMismatch_DeviceAttachedToNil(t *testing.T) {
	mgr, p, _ := setupTestManager(t)
	ctx := context.Background()
	
	// Set up liveness checker
	liveness := newMockLivenessChecker()
	mgr.livenessChecker = liveness
	
	instanceID := "instance-123"
	deviceID := "device-abc"
	
	// Create device with NO AttachedTo
	device := &Device{
		Id:         deviceID,
		Name:       "test-gpu",
		Type:       DeviceTypeGPU,
		PCIAddress: "0000:99:00.0",
		VendorID:   "10de",
		DeviceID:   "1234",
		AttachedTo: nil, // Not attached according to device metadata
		CreatedAt:  time.Now(),
	}
	createTestDevice(t, p, device)
	
	// Instance claims to have this device
	liveness.setInstanceDevices(instanceID, []string{deviceID})
	liveness.setRunning(instanceID, true)
	
	// Run reconciliation - should log mismatch but not error
	err := mgr.ReconcileDevices(ctx)
	require.NoError(t, err)
	// Note: This is a log-only mismatch, device state should remain unchanged
	
	updatedDevice, err := mgr.loadDevice(deviceID)
	require.NoError(t, err)
	assert.Nil(t, updatedDevice.AttachedTo, "Device should remain unattached (log-only mismatch)")
}

func TestReconcileDevices_TwoWayMismatch_DeviceAttachedToWrongInstance(t *testing.T) {
	mgr, p, _ := setupTestManager(t)
	ctx := context.Background()
	
	// Set up liveness checker
	liveness := newMockLivenessChecker()
	mgr.livenessChecker = liveness
	
	instanceID1 := "instance-1"
	instanceID2 := "instance-2"
	deviceID := "device-abc"
	
	// Create device attached to instance-1
	device := &Device{
		Id:         deviceID,
		Name:       "test-gpu",
		Type:       DeviceTypeGPU,
		PCIAddress: "0000:99:00.0",
		VendorID:   "10de",
		DeviceID:   "1234",
		AttachedTo: &instanceID1, // Attached to instance-1
		CreatedAt:  time.Now(),
	}
	createTestDevice(t, p, device)
	
	// Both instances exist and are running
	createTestInstanceDir(t, p, instanceID1)
	createTestInstanceDir(t, p, instanceID2)
	liveness.setRunning(instanceID1, true)
	liveness.setRunning(instanceID2, true)
	
	// instance-2 claims to have this device (mismatch!)
	liveness.setInstanceDevices(instanceID2, []string{deviceID})
	
	// Run reconciliation - should log mismatch but not error
	err := mgr.ReconcileDevices(ctx)
	require.NoError(t, err)
	// Note: This is a log-only mismatch, device state should remain unchanged
	
	updatedDevice, err := mgr.loadDevice(deviceID)
	require.NoError(t, err)
	require.NotNil(t, updatedDevice.AttachedTo)
	assert.Equal(t, instanceID1, *updatedDevice.AttachedTo, "Device should remain attached to original instance (log-only mismatch)")
}

func TestReconcileDevices_MultipleDevices(t *testing.T) {
	mgr, p, _ := setupTestManager(t)
	ctx := context.Background()
	
	// Set up liveness checker
	liveness := newMockLivenessChecker()
	mgr.livenessChecker = liveness
	
	runningInstanceID := "running-instance"
	stoppedInstanceID := "stopped-instance"
	orphanedInstanceID := "orphaned-instance"
	
	// Device 1: Attached to running instance - should stay attached
	device1 := &Device{
		Id:         "device-1",
		Name:       "gpu-1",
		Type:       DeviceTypeGPU,
		PCIAddress: "0000:01:00.0",
		VendorID:   "10de",
		DeviceID:   "1234",
		AttachedTo: &runningInstanceID,
		CreatedAt:  time.Now(),
	}
	
	// Device 2: Attached to stopped instance - should be cleared
	device2 := &Device{
		Id:         "device-2",
		Name:       "gpu-2",
		Type:       DeviceTypeGPU,
		PCIAddress: "0000:02:00.0",
		VendorID:   "10de",
		DeviceID:   "5678",
		AttachedTo: &stoppedInstanceID,
		CreatedAt:  time.Now(),
	}
	
	// Device 3: Attached to non-existent instance - should be cleared
	device3 := &Device{
		Id:         "device-3",
		Name:       "gpu-3",
		Type:       DeviceTypeGPU,
		PCIAddress: "0000:03:00.0",
		VendorID:   "10de",
		DeviceID:   "9abc",
		AttachedTo: &orphanedInstanceID,
		CreatedAt:  time.Now(),
	}
	
	// Device 4: Not attached - should stay unattached
	device4 := &Device{
		Id:         "device-4",
		Name:       "gpu-4",
		Type:       DeviceTypeGPU,
		PCIAddress: "0000:04:00.0",
		VendorID:   "10de",
		DeviceID:   "def0",
		AttachedTo: nil,
		CreatedAt:  time.Now(),
	}
	
	createTestDevice(t, p, device1)
	createTestDevice(t, p, device2)
	createTestDevice(t, p, device3)
	createTestDevice(t, p, device4)
	
	// Set up instance states
	createTestInstanceDir(t, p, runningInstanceID)
	createTestInstanceDir(t, p, stoppedInstanceID)
	// Don't create orphanedInstanceID directory
	
	liveness.setRunning(runningInstanceID, true)
	liveness.setRunning(stoppedInstanceID, false)
	// orphanedInstanceID doesn't exist in liveness checker
	
	// Run reconciliation
	err := mgr.ReconcileDevices(ctx)
	require.NoError(t, err)
	
	// Verify device 1 stays attached (running instance)
	d1, err := mgr.loadDevice("device-1")
	require.NoError(t, err)
	require.NotNil(t, d1.AttachedTo)
	assert.Equal(t, runningInstanceID, *d1.AttachedTo)
	
	// Verify device 2 is cleared (stopped instance)
	d2, err := mgr.loadDevice("device-2")
	require.NoError(t, err)
	assert.Nil(t, d2.AttachedTo)
	
	// Verify device 3 is cleared (orphaned instance)
	d3, err := mgr.loadDevice("device-3")
	require.NoError(t, err)
	assert.Nil(t, d3.AttachedTo)
	
	// Verify device 4 stays unattached
	d4, err := mgr.loadDevice("device-4")
	require.NoError(t, err)
	assert.Nil(t, d4.AttachedTo)
}

func TestSetLivenessChecker(t *testing.T) {
	mgr, _, _ := setupTestManager(t)
	
	// Initially nil
	assert.Nil(t, mgr.livenessChecker)
	
	// Set liveness checker
	liveness := newMockLivenessChecker()
	mgr.SetLivenessChecker(liveness)
	
	// Verify it was set
	assert.Equal(t, liveness, mgr.livenessChecker)
}

func TestIsInstanceOrphaned_NoLivenessChecker(t *testing.T) {
	mgr, p, _ := setupTestManager(t)
	ctx := context.Background()
	
	existingInstanceID := "existing-instance"
	missingInstanceID := "missing-instance"
	
	// Create one instance directory
	createTestInstanceDir(t, p, existingInstanceID)
	
	// Existing instance is NOT orphaned
	assert.False(t, mgr.isInstanceOrphaned(ctx, existingInstanceID))
	
	// Missing instance IS orphaned
	assert.True(t, mgr.isInstanceOrphaned(ctx, missingInstanceID))
}

func TestIsInstanceOrphaned_WithLivenessChecker(t *testing.T) {
	mgr, p, _ := setupTestManager(t)
	ctx := context.Background()
	
	// Set up liveness checker
	liveness := newMockLivenessChecker()
	mgr.livenessChecker = liveness
	
	runningInstanceID := "running-instance"
	stoppedInstanceID := "stopped-instance"
	
	// Both instances have directories
	createTestInstanceDir(t, p, runningInstanceID)
	createTestInstanceDir(t, p, stoppedInstanceID)
	
	liveness.setRunning(runningInstanceID, true)
	liveness.setRunning(stoppedInstanceID, false)
	
	// Running instance is NOT orphaned
	assert.False(t, mgr.isInstanceOrphaned(ctx, runningInstanceID))
	
	// Stopped instance IS orphaned (even though directory exists)
	assert.True(t, mgr.isInstanceOrphaned(ctx, stoppedInstanceID))
}

func TestReconcileDevices_NoDevicesDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	p := paths.New(tmpDir)
	
	// Don't create devices directory
	
	mgr := &manager{
		paths:      p,
		vfioBinder: NewVFIOBinder(),
	}
	
	ctx := context.Background()
	
	// Should not error when directory doesn't exist
	err := mgr.ReconcileDevices(ctx)
	require.NoError(t, err)
}

func TestReconcileStats(t *testing.T) {
	// Verify stats struct has expected fields
	stats := reconcileStats{}
	
	stats.orphanedCleared = 1
	stats.resetAttempted = 2
	stats.resetSucceeded = 3
	stats.resetFailed = 4
	stats.mismatches = 5
	stats.suspiciousVMM = 6
	stats.errors = 7
	
	assert.Equal(t, 1, stats.orphanedCleared)
	assert.Equal(t, 2, stats.resetAttempted)
	assert.Equal(t, 3, stats.resetSucceeded)
	assert.Equal(t, 4, stats.resetFailed)
	assert.Equal(t, 5, stats.mismatches)
	assert.Equal(t, 6, stats.suspiciousVMM)
	assert.Equal(t, 7, stats.errors)
}

// TestResetOrphanedDevice_NonExistentPCIAddress tests that reset-lite
// handles non-existent PCI addresses gracefully (doesn't panic)
func TestResetOrphanedDevice_NonExistentPCIAddress(t *testing.T) {
	mgr, p, _ := setupTestManager(t)
	ctx := context.Background()
	
	// Create device with fake PCI address that doesn't exist
	device := &Device{
		Id:          "test-device",
		Name:        "test-gpu",
		Type:        DeviceTypeGPU,
		PCIAddress:  "0000:ff:ff.f", // Non-existent
		VendorID:    "10de",         // NVIDIA vendor ID
		DeviceID:    "1234",
		BoundToVFIO: true, // Claim it's bound to VFIO
		CreatedAt:   time.Now(),
	}
	createTestDevice(t, p, device)
	
	stats := &reconcileStats{}
	
	// Should not panic, should handle errors gracefully
	mgr.resetOrphanedDevice(ctx, device, stats)
	
	// Reset was attempted
	assert.Equal(t, 1, stats.resetAttempted)
	
	// May fail due to non-existent device, that's expected
	// The key is it doesn't panic
}

// Helper function for testing: verify device directory structure
func verifyDeviceDir(t *testing.T, p *paths.Paths, deviceID string) bool {
	t.Helper()
	metadataPath := p.DeviceMetadata(deviceID)
	_, err := os.Stat(metadataPath)
	return err == nil
}

// TestReconcileDevices_CorruptedDeviceMetadata tests handling of
// corrupted device metadata files
func TestReconcileDevices_CorruptedDeviceMetadata(t *testing.T) {
	mgr, p, _ := setupTestManager(t)
	ctx := context.Background()
	
	// Create a valid device
	validDevice := &Device{
		Id:         "valid-device",
		Name:       "valid-gpu",
		Type:       DeviceTypeGPU,
		PCIAddress: "0000:01:00.0",
		VendorID:   "10de",
		DeviceID:   "1234",
		CreatedAt:  time.Now(),
	}
	createTestDevice(t, p, validDevice)
	
	// Create a corrupted device directory with invalid JSON
	corruptedID := "corrupted-device"
	corruptedDir := p.DeviceDir(corruptedID)
	require.NoError(t, os.MkdirAll(corruptedDir, 0755))
	corruptedPath := filepath.Join(corruptedDir, "metadata.json")
	require.NoError(t, os.WriteFile(corruptedPath, []byte("not valid json{{{"), 0644))
	
	// Should not error - should skip corrupted device and continue
	err := mgr.ReconcileDevices(ctx)
	require.NoError(t, err)
	
	// Valid device should still be loadable
	d, err := mgr.loadDevice("valid-device")
	require.NoError(t, err)
	assert.Equal(t, "valid-gpu", d.Name)
}

