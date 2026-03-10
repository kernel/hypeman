package api

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/c2h5oh/datasize"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/instances"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListInstances_Empty(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	resp, err := svc.ListInstances(ctx(), oapi.ListInstancesRequestObject{})
	require.NoError(t, err)

	list, ok := resp.(oapi.ListInstances200JSONResponse)
	require.True(t, ok, "expected 200 response")
	assert.Empty(t, list)
}

func TestGetInstance_NotFound(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	// With middleware, not-found would be handled before reaching handler.
	// For this test, we call the manager directly to verify the error type.
	_, err := svc.InstanceManager.GetInstance(ctx(), "non-existent")
	require.Error(t, err)
}

func TestCreateInstance_ParsesHumanReadableSizes(t *testing.T) {
	t.Parallel()
	// Require KVM access for VM creation
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available, skipping on this platform")
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
				BandwidthDownload *string `json:"bandwidth_download,omitempty"`
				BandwidthUpload   *string `json:"bandwidth_upload,omitempty"`
				Enabled           *bool   `json:"enabled,omitempty"`
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
	t.Parallel()
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
				BandwidthDownload *string `json:"bandwidth_download,omitempty"`
				BandwidthUpload   *string `json:"bandwidth_upload,omitempty"`
				Enabled           *bool   `json:"enabled,omitempty"`
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

type captureCreateManager struct {
	instances.Manager
	lastReq *instances.CreateInstanceRequest
}

type captureForkManager struct {
	instances.Manager
	lastID  string
	lastReq *instances.ForkInstanceRequest
	result  *instances.Instance
	err     error
}

func (m *captureForkManager) ForkInstance(ctx context.Context, id string, req instances.ForkInstanceRequest) (*instances.Instance, error) {
	reqCopy := req
	m.lastID = id
	m.lastReq = &reqCopy
	if m.err != nil {
		return nil, m.err
	}
	return m.result, nil
}

func (m *captureCreateManager) CreateInstance(ctx context.Context, req instances.CreateInstanceRequest) (*instances.Instance, error) {
	reqCopy := req
	m.lastReq = &reqCopy

	now := time.Now()
	return &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-hotplug-default",
			Name:           req.Name,
			Image:          req.Image,
			Size:           req.Size,
			HotplugSize:    req.HotplugSize,
			OverlaySize:    req.OverlaySize,
			Vcpus:          req.Vcpus,
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateRunning,
	}, nil
}

func TestCreateInstance_OmittedHotplugSizeDefaultsToZero(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	origMgr := svc.InstanceManager
	mockMgr := &captureCreateManager{Manager: origMgr}
	svc.InstanceManager = mockMgr

	size := "1GB"
	overlaySize := "10GB"
	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:        "test-no-hotplug",
			Image:       "docker.io/library/alpine:latest",
			Size:        &size,
			OverlaySize: &overlaySize,
		},
	})
	require.NoError(t, err)

	created, ok := resp.(oapi.CreateInstance201JSONResponse)
	require.True(t, ok, "expected 201 response")
	assert.NotNil(t, mockMgr.lastReq, "CreateInstance should be called")
	assert.Equal(t, int64(0), mockMgr.lastReq.HotplugSize, "omitted hotplug_size should not allocate default memory")

	instance := oapi.Instance(created)
	require.NotNil(t, instance.HotplugSize)

	var hotplugBytes datasize.ByteSize
	require.NoError(t, hotplugBytes.UnmarshalText([]byte(*instance.HotplugSize)))
	assert.Equal(t, int64(0), int64(hotplugBytes), "response should report zero hotplug_size when omitted")
}

func TestForkInstance_Success(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	now := time.Now()
	source := instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "src-instance",
			Name:           "src-instance",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateStopped,
	}

	forked := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "forked-instance",
			Name:           "forked-instance",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateStopped,
	}

	mockMgr := &captureForkManager{
		Manager: svc.InstanceManager,
		result:  forked,
	}
	svc.InstanceManager = mockMgr

	resp, err := svc.ForkInstance(
		mw.WithResolvedInstance(ctx(), source.Id, source),
		oapi.ForkInstanceRequestObject{
			Id: source.Id,
			Body: &oapi.ForkInstanceRequest{
				Name: "forked-instance",
			},
		},
	)
	require.NoError(t, err)

	created, ok := resp.(oapi.ForkInstance201JSONResponse)
	require.True(t, ok, "expected 201 response")
	assert.Equal(t, "forked-instance", created.Name)
	assert.Equal(t, source.Id, mockMgr.lastID)
	require.NotNil(t, mockMgr.lastReq)
	assert.Equal(t, "forked-instance", mockMgr.lastReq.Name)
	assert.False(t, mockMgr.lastReq.FromRunning)
	assert.Equal(t, instances.State(""), mockMgr.lastReq.TargetState)
}

func TestForkInstance_NotSupported(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	source := instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "src-instance",
			Name:           "src-instance",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      time.Now(),
			HypervisorType: hypervisor.TypeQEMU,
		},
		State: instances.StateStopped,
	}

	mockMgr := &captureForkManager{
		Manager: svc.InstanceManager,
		err:     instances.ErrNotSupported,
	}
	svc.InstanceManager = mockMgr

	resp, err := svc.ForkInstance(
		mw.WithResolvedInstance(ctx(), source.Id, source),
		oapi.ForkInstanceRequestObject{
			Id: source.Id,
			Body: &oapi.ForkInstanceRequest{
				Name: "forked-instance",
			},
		},
	)
	require.NoError(t, err)

	notSupported, ok := resp.(oapi.ForkInstance501JSONResponse)
	require.True(t, ok, "expected 501 response")
	assert.Equal(t, "not_supported", notSupported.Code)
}

func TestForkInstance_InvalidRequest(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	source := instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "src-instance",
			Name:           "src-instance",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      time.Now(),
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateStopped,
	}

	mockMgr := &captureForkManager{
		Manager: svc.InstanceManager,
		err:     fmt.Errorf("%w: name is required", instances.ErrInvalidRequest),
	}
	svc.InstanceManager = mockMgr

	resp, err := svc.ForkInstance(
		mw.WithResolvedInstance(ctx(), source.Id, source),
		oapi.ForkInstanceRequestObject{
			Id: source.Id,
			Body: &oapi.ForkInstanceRequest{
				Name: "",
			},
		},
	)
	require.NoError(t, err)

	badReq, ok := resp.(oapi.ForkInstance400JSONResponse)
	require.True(t, ok, "expected 400 response")
	assert.Equal(t, "invalid_request", badReq.Code)
}

func TestForkInstance_FromRunningFlagForwarded(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	now := time.Now()
	source := instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "src-instance",
			Name:           "src-instance",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateRunning,
	}

	forked := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "forked-instance",
			Name:           "forked-instance",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateStandby,
	}

	mockMgr := &captureForkManager{
		Manager: svc.InstanceManager,
		result:  forked,
	}
	svc.InstanceManager = mockMgr

	fromRunning := true
	targetState := oapi.ForkTargetStateRunning
	resp, err := svc.ForkInstance(
		mw.WithResolvedInstance(ctx(), source.Id, source),
		oapi.ForkInstanceRequestObject{
			Id: source.Id,
			Body: &oapi.ForkInstanceRequest{
				Name:        "forked-instance",
				FromRunning: &fromRunning,
				TargetState: &targetState,
			},
		},
	)
	require.NoError(t, err)

	_, ok := resp.(oapi.ForkInstance201JSONResponse)
	require.True(t, ok, "expected 201 response")
	require.NotNil(t, mockMgr.lastReq)
	assert.True(t, mockMgr.lastReq.FromRunning)
	assert.Equal(t, instances.StateRunning, mockMgr.lastReq.TargetState)
}

func TestInstanceLifecycle_StopStart(t *testing.T) {
	t.Parallel()
	// Require KVM access for VM creation
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available - skipping lifecycle test")
	}

	svc := newTestService(t)

	// Use nginx:alpine so the VM runs a real workload (not just exits immediately)
	createAndWaitForImage(t, svc, "docker.io/library/nginx:alpine", 60*time.Second)

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
			Image: "docker.io/library/nginx:alpine",
			Network: &struct {
				BandwidthDownload *string `json:"bandwidth_download,omitempty"`
				BandwidthUpload   *string `json:"bandwidth_upload,omitempty"`
				Enabled           *bool   `json:"enabled,omitempty"`
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
	stopResp, err := svc.StopInstance(ctxWithInstance(svc, instanceID), oapi.StopInstanceRequestObject{Id: instanceID})
	require.NoError(t, err)

	stopped, ok := stopResp.(oapi.StopInstance200JSONResponse)
	require.True(t, ok, "expected 200 response for stop, got %T", stopResp)
	assert.Equal(t, oapi.InstanceState("Stopped"), stopped.State)
	t.Log("Instance stopped successfully")

	// 3. Start the instance
	t.Log("Starting instance...")
	startResp, err := svc.StartInstance(ctxWithInstance(svc, instanceID), oapi.StartInstanceRequestObject{Id: instanceID})
	require.NoError(t, err)

	started, ok := startResp.(oapi.StartInstance200JSONResponse)
	require.True(t, ok, "expected 200 response for start, got %T", startResp)
	t.Logf("Instance started (state: %s)", started.State)

	// Wait for Running state after start
	waitForState(t, svc, instanceID, "Running", 30*time.Second)

	// 4. Cleanup - delete the instance
	t.Log("Deleting instance...")
	deleteResp, err := svc.DeleteInstance(ctxWithInstance(svc, instanceID), oapi.DeleteInstanceRequestObject{Id: instanceID})
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
		// Use manager directly to poll state (middleware not needed for polling)
		inst, err := svc.InstanceManager.GetInstance(ctx(), instanceID)
		require.NoError(t, err)

		if string(inst.State) == expectedState {
			t.Logf("Instance reached %s state", expectedState)
			return
		}
		t.Logf("Instance state: %s (waiting for %s)", inst.State, expectedState)
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("Timeout waiting for instance to reach %s state", expectedState)
}
