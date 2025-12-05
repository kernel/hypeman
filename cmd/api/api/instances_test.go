package api

import (
	"os"
	"testing"
	"time"

	"github.com/onkernel/hypeman/lib/oapi"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListInstances_Empty(t *testing.T) {
	svc := newTestService(t)

	resp, err := svc.ListInstances(ctx(), oapi.ListInstancesRequestObject{})
	require.NoError(t, err)

	list, ok := resp.(oapi.ListInstances200JSONResponse)
	require.True(t, ok, "expected 200 response")
	assert.Empty(t, list)
}

func TestGetInstance_NotFound(t *testing.T) {
	svc := newTestService(t)

	resp, err := svc.GetInstance(ctx(), oapi.GetInstanceRequestObject{
		Id: "non-existent",
	})
	require.NoError(t, err)

	notFound, ok := resp.(oapi.GetInstance404JSONResponse)
	require.True(t, ok, "expected 404 response")
	assert.Equal(t, "not_found", notFound.Code)
}

func TestCreateInstance_ParsesHumanReadableSizes(t *testing.T) {
	// Require KVM access for VM creation
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Fatal("/dev/kvm not available - ensure KVM is enabled and user is in 'kvm' group (sudo usermod -aG kvm $USER)")
	}

	svc := newTestService(t)

	// Create and wait for alpine image
	createAndWaitForImage(t, svc, "docker.io/library/alpine:latest", 30*time.Second)

	// Ensure system files (kernel and initramfs) are available
	t.Log("Ensuring system files (kernel and initramfs)...")
	systemMgr := system.NewManager(paths.New(svc.Config.DataDir))
	err := systemMgr.EnsureSystemFiles(ctx())
	require.NoError(t, err)
	t.Log("System files ready!")

	// Now test instance creation with human-readable size strings
	size := "512MB"
	hotplugSize := "1GB"
	overlaySize := "5GB"

	t.Log("Creating instance with human-readable sizes...")
	networkEnabled := false
	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:        "test-sizes",
			Image:       "docker.io/library/alpine:latest",
			Size:        &size,
			HotplugSize: &hotplugSize,
			OverlaySize: &overlaySize,
			Network: &struct {
				Enabled *bool `json:"enabled,omitempty"`
			}{
				Enabled: &networkEnabled,
			},
		},
	})
	require.NoError(t, err)

	// Should successfully create the instance
	created, ok := resp.(oapi.CreateInstance201JSONResponse)
	require.True(t, ok, "expected 201 response")

	instance := oapi.Instance(created)

	// Verify the instance was created with our sizes
	assert.Equal(t, "test-sizes", instance.Name)
	assert.NotNil(t, instance.Size)
	assert.NotNil(t, instance.HotplugSize)
	assert.NotNil(t, instance.OverlaySize)

	// Verify sizes are formatted as human-readable strings (not raw bytes)
	t.Logf("Response sizes: size=%s, hotplug_size=%s, overlay_size=%s",
		*instance.Size, *instance.HotplugSize, *instance.OverlaySize)

	// Verify exact formatted output from the API
	// Note: 1GB (1073741824 bytes) is formatted as 1024.0 MB by the .HR() method
	assert.Equal(t, "512.0 MB", *instance.Size, "size should be formatted as 512.0 MB")
	assert.Equal(t, "1024.0 MB", *instance.HotplugSize, "hotplug_size should be formatted as 1024.0 MB (1GB)")
	assert.Equal(t, "5.0 GB", *instance.OverlaySize, "overlay_size should be formatted as 5.0 GB")
}

func TestCreateInstance_InvalidSizeFormat(t *testing.T) {
	svc := newTestService(t)

	// Test with invalid size format
	invalidSize := "not-a-size"
	networkEnabled := false

	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:  "test-invalid",
			Image: "docker.io/library/alpine:latest",
			Size:  &invalidSize,
			Network: &struct {
				Enabled *bool `json:"enabled,omitempty"`
			}{
				Enabled: &networkEnabled,
			},
		},
	})
	require.NoError(t, err)

	// Should get invalid_size error
	badReq, ok := resp.(oapi.CreateInstance400JSONResponse)
	require.True(t, ok, "expected 400 response")
	assert.Equal(t, "invalid_size", badReq.Code)
	assert.Contains(t, badReq.Message, "invalid size format")
}

func TestInstanceLifecycle_StopStartReboot(t *testing.T) {
	// Require KVM access for VM creation
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available - skipping lifecycle test")
	}

	svc := newTestService(t)

	// Create and wait for alpine image
	createAndWaitForImage(t, svc, "docker.io/library/alpine:latest", 30*time.Second)

	// Ensure system files (kernel and initramfs) are available
	t.Log("Ensuring system files (kernel and initramfs)...")
	systemMgr := system.NewManager(paths.New(svc.Config.DataDir))
	err := systemMgr.EnsureSystemFiles(ctx())
	require.NoError(t, err)
	t.Log("System files ready!")

	// 1. Create instance
	t.Log("Creating instance...")
	networkEnabled := false
	createResp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:  "test-lifecycle",
			Image: "docker.io/library/alpine:latest",
			Network: &struct {
				Enabled *bool `json:"enabled,omitempty"`
			}{
				Enabled: &networkEnabled,
			},
		},
	})
	require.NoError(t, err)

	created, ok := createResp.(oapi.CreateInstance201JSONResponse)
	require.True(t, ok, "expected 201 response for create")

	instance := oapi.Instance(created)
	instanceID := instance.Id
	t.Logf("Instance created: %s (state: %s)", instanceID, instance.State)

	// Verify instance reaches Running state
	waitForState(t, svc, instanceID, "Running", 30*time.Second)

	// 2. Stop the instance
	t.Log("Stopping instance...")
	stopResp, err := svc.StopInstance(ctx(), oapi.StopInstanceRequestObject{Id: instanceID})
	require.NoError(t, err)

	stopped, ok := stopResp.(oapi.StopInstance200JSONResponse)
	require.True(t, ok, "expected 200 response for stop, got %T", stopResp)
	assert.Equal(t, oapi.InstanceState("Stopped"), stopped.State)
	t.Log("Instance stopped successfully")

	// 3. Start the instance
	t.Log("Starting instance...")
	startResp, err := svc.StartInstance(ctx(), oapi.StartInstanceRequestObject{Id: instanceID})
	require.NoError(t, err)

	started, ok := startResp.(oapi.StartInstance200JSONResponse)
	require.True(t, ok, "expected 200 response for start, got %T", startResp)
	t.Logf("Instance started (state: %s)", started.State)

	// Wait for Running state after start
	waitForState(t, svc, instanceID, "Running", 30*time.Second)

	// 4. Reboot the instance
	t.Log("Rebooting instance...")
	rebootResp, err := svc.RebootInstance(ctx(), oapi.RebootInstanceRequestObject{Id: instanceID})
	require.NoError(t, err)

	rebooted, ok := rebootResp.(oapi.RebootInstance200JSONResponse)
	require.True(t, ok, "expected 200 response for reboot, got %T", rebootResp)
	assert.Equal(t, oapi.InstanceState("Running"), rebooted.State)
	t.Log("Instance rebooted successfully")

	// 5. Cleanup - delete the instance
	t.Log("Deleting instance...")
	deleteResp, err := svc.DeleteInstance(ctx(), oapi.DeleteInstanceRequestObject{Id: instanceID})
	require.NoError(t, err)
	_, ok = deleteResp.(oapi.DeleteInstance204Response)
	require.True(t, ok, "expected 204 response for delete")
	t.Log("Instance deleted successfully")
}

// waitForState polls until instance reaches the expected state or times out
func waitForState(t *testing.T, svc *ApiService, instanceID string, expectedState string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := svc.GetInstance(ctx(), oapi.GetInstanceRequestObject{Id: instanceID})
		require.NoError(t, err)

		if inst, ok := resp.(oapi.GetInstance200JSONResponse); ok {
			if string(inst.State) == expectedState {
				t.Logf("Instance reached %s state", expectedState)
				return
			}
			t.Logf("Instance state: %s (waiting for %s)", inst.State, expectedState)
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("Timeout waiting for instance to reach %s state", expectedState)
}
