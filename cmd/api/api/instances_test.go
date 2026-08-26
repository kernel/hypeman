package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/c2h5oh/datasize"
	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/healthcheck"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/paths"
	restartpolicy "github.com/kernel/hypeman/lib/restart-policy"
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

func TestCreateInstance_AutoPullImage(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available, skipping on this platform")
	}

	svc := newTestService(t)

	// NOTE: intentionally NOT calling createAndWaitForImage here.
	// The auto-pull logic in CreateInstance should handle pulling the image.
	// Use the test registry mirror when configured so this covers auto-pull
	// orchestration without depending on live Docker Hub auth latency.

	// Ensure system files (kernel and initramfs) are available
	t.Log("Ensuring system files (kernel and initramfs)...")
	systemMgr := system.NewManager(paths.New(svc.Config.DataDir))
	err := systemMgr.EnsureSystemFiles(ctx())
	require.NoError(t, err)

	t.Log("Creating instance without pre-pulling image (testing auto-pull)...")
	networkEnabled := false
	createReq := oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:  "test-auto-pull",
			Image: apiTestImageRef(t, "docker.io/library/alpine:latest"),
			Network: &struct {
				BandwidthDownload *string                                  `json:"bandwidth_download,omitempty"`
				BandwidthUpload   *string                                  `json:"bandwidth_upload,omitempty"`
				Egress            *oapi.CreateInstanceRequestNetworkEgress `json:"egress,omitempty"`
				Enabled           *bool                                    `json:"enabled,omitempty"`
			}{
				Enabled: &networkEnabled,
			},
		},
	}

	deadline := time.Now().Add(integrationTestTimeout(15 * time.Second))
	var created oapi.CreateInstance201JSONResponse
	for {
		resp, err := svc.CreateInstance(ctx(), createReq)
		require.NoError(t, err)

		if okResp, ok := resp.(oapi.CreateInstance201JSONResponse); ok {
			created = okResp
			break
		}

		notReadyResp, ok := resp.(oapi.CreateInstance400JSONResponse)
		require.True(t, ok, "expected create to either succeed or report image_not_ready while auto-pull finishes")
		require.Equal(t, "image_not_ready", notReadyResp.Code)

		if time.Now().After(deadline) {
			t.Fatalf("auto-pull did not finish before deadline: %s", notReadyResp.Message)
		}
		time.Sleep(500 * time.Millisecond)
	}

	t.Logf("Instance created via auto-pull: %s", created.Id)

	// Cleanup: delete the instance
	instanceID := created.Id
	t.Log("Deleting instance...")
	deleteResp, err := svc.DeleteInstance(ctxWithInstance(svc, instanceID), oapi.DeleteInstanceRequestObject{Id: instanceID})
	require.NoError(t, err)
	_, ok := deleteResp.(oapi.DeleteInstance204Response)
	require.True(t, ok, "expected 204 response for delete")
	t.Log("Instance deleted successfully")
}

func TestCreateInstance_ParsesHumanReadableSizes(t *testing.T) {
	t.Parallel()
	// Require KVM access for VM creation
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available, skipping on this platform")
	}

	svc := newTestService(t)

	// Create and wait for alpine image
	imageName := createAndWaitForImage(t, svc, "docker.io/library/alpine:latest", 30*time.Second)

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
			Image:       imageName,
			Size:        &size,
			HotplugSize: &hotplugSize,
			OverlaySize: &overlaySize,
			Network: &struct {
				BandwidthDownload *string                                  `json:"bandwidth_download,omitempty"`
				BandwidthUpload   *string                                  `json:"bandwidth_upload,omitempty"`
				Egress            *oapi.CreateInstanceRequestNetworkEgress `json:"egress,omitempty"`
				Enabled           *bool                                    `json:"enabled,omitempty"`
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
				BandwidthDownload *string                                  `json:"bandwidth_download,omitempty"`
				BandwidthUpload   *string                                  `json:"bandwidth_upload,omitempty"`
				Egress            *oapi.CreateInstanceRequestNetworkEgress `json:"egress,omitempty"`
				Enabled           *bool                                    `json:"enabled,omitempty"`
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

type captureManager[Req any] struct {
	instances.Manager
	lastID  string
	lastReq *Req
	result  *instances.Instance
	err     error
}

func newCaptureManager[Req any](manager instances.Manager) captureManager[Req] {
	return captureManager[Req]{Manager: manager}
}

func (m *captureManager[Req]) capture(id string, req Req) (*instances.Instance, error) {
	reqCopy := req
	m.lastID = id
	m.lastReq = &reqCopy
	if m.err != nil {
		return nil, m.err
	}
	return m.result, nil
}

type captureCreateManager struct {
	captureManager[instances.CreateInstanceRequest]
}

func newCaptureCreateManager(manager instances.Manager) *captureCreateManager {
	return &captureCreateManager{captureManager: newCaptureManager[instances.CreateInstanceRequest](manager)}
}

type captureForkManager struct {
	captureManager[instances.ForkInstanceRequest]
}

func newCaptureForkManager(manager instances.Manager) *captureForkManager {
	return &captureForkManager{captureManager: newCaptureManager[instances.ForkInstanceRequest](manager)}
}

type captureStandbyManager struct {
	captureManager[instances.StandbyInstanceRequest]
}

func newCaptureStandbyManager(manager instances.Manager) *captureStandbyManager {
	return &captureStandbyManager{captureManager: newCaptureManager[instances.StandbyInstanceRequest](manager)}
}

type captureUpdateManager struct {
	captureManager[instances.UpdateInstanceRequest]
}

func newCaptureUpdateManager(manager instances.Manager) *captureUpdateManager {
	return &captureUpdateManager{captureManager: newCaptureManager[instances.UpdateInstanceRequest](manager)}
}

func (m *captureForkManager) ForkInstance(ctx context.Context, id string, req instances.ForkInstanceRequest) (*instances.Instance, error) {
	return m.capture(id, req)
}

func (m *captureStandbyManager) StandbyInstance(ctx context.Context, id string, req instances.StandbyInstanceRequest) (*instances.Instance, error) {
	return m.capture(id, req)
}

func (m *captureUpdateManager) UpdateInstance(ctx context.Context, id string, req instances.UpdateInstanceRequest) (*instances.Instance, error) {
	result, err := m.capture(id, req)
	if err != nil || result != nil {
		return result, err
	}

	now := time.Now()
	expiresAt := req.ExpiresAt
	if req.TTL != nil && *req.TTL > 0 {
		resolved := now.Add(*req.TTL)
		expiresAt = &resolved
	}
	return &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             id,
			Name:           "updated-instance",
			Image:          "docker.io/library/alpine:latest",
			Env:            req.Env,
			AutoStandby:    req.AutoStandby,
			HealthCheck:    req.HealthCheck,
			RestartPolicy:  req.RestartPolicy,
			CreatedAt:      now,
			ExpiresAt:      expiresAt,
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateRunning,
	}, nil
}

func (m *captureCreateManager) CreateInstance(ctx context.Context, req instances.CreateInstanceRequest) (*instances.Instance, error) {
	result, err := m.capture("", req)
	if err != nil || result != nil {
		return result, err
	}

	now := time.Now()
	expiresAt := req.ExpiresAt
	if req.TTL != nil && *req.TTL > 0 {
		resolved := now.Add(*req.TTL)
		expiresAt = &resolved
	}
	return &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-hotplug-default",
			Name:           req.Name,
			Image:          req.Image,
			Size:           req.Size,
			HotplugSize:    req.HotplugSize,
			OverlaySize:    req.OverlaySize,
			Vcpus:          req.Vcpus,
			AutoStandby:    req.AutoStandby,
			HealthCheck:    req.HealthCheck,
			RestartPolicy:  req.RestartPolicy,
			CreatedAt:      now,
			ExpiresAt:      expiresAt,
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateRunning,
	}, nil
}

func TestCreateInstance_TTL(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	mockMgr := newCaptureCreateManager(svc.InstanceManager)
	svc.InstanceManager = mockMgr
	ttl := "90m"

	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{Body: &oapi.CreateInstanceRequest{
		Name:  "test-ttl",
		Image: "docker.io/library/alpine:latest",
		Ttl:   &ttl,
	}})
	require.NoError(t, err)
	created, ok := resp.(oapi.CreateInstance201JSONResponse)
	require.True(t, ok)
	require.NotNil(t, mockMgr.lastReq)
	require.NotNil(t, mockMgr.lastReq.TTL)
	assert.Equal(t, 90*time.Minute, *mockMgr.lastReq.TTL)
	require.NotNil(t, created.ExpiresAt)
	assert.Equal(t, created.CreatedAt.Add(90*time.Minute), *created.ExpiresAt)
}

func TestCreateInstance_ZeroTTLDisablesExpiration(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	mockMgr := newCaptureCreateManager(svc.InstanceManager)
	svc.InstanceManager = mockMgr
	ttl := "0s"

	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{Body: &oapi.CreateInstanceRequest{
		Name:  "test-no-expiration",
		Image: "docker.io/library/alpine:latest",
		Ttl:   &ttl,
	}})
	require.NoError(t, err)
	created, ok := resp.(oapi.CreateInstance201JSONResponse)
	require.True(t, ok)
	require.NotNil(t, mockMgr.lastReq.TTL)
	assert.Zero(t, *mockMgr.lastReq.TTL)
	assert.Nil(t, created.ExpiresAt)
}

func TestCreateInstance_ExpiresAt(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	mockMgr := newCaptureCreateManager(svc.InstanceManager)
	svc.InstanceManager = mockMgr
	expiresAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{Body: &oapi.CreateInstanceRequest{
		Name:      "test-expires-at",
		Image:     "docker.io/library/alpine:latest",
		ExpiresAt: &expiresAt,
	}})
	require.NoError(t, err)
	created, ok := resp.(oapi.CreateInstance201JSONResponse)
	require.True(t, ok)
	require.NotNil(t, mockMgr.lastReq.ExpiresAt)
	assert.Equal(t, expiresAt, *mockMgr.lastReq.ExpiresAt)
	require.NotNil(t, created.ExpiresAt)
	assert.Equal(t, expiresAt, *created.ExpiresAt)
}

func TestCreateInstance_MapsInvalidExpiresAt(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	mockMgr := newCaptureCreateManager(svc.InstanceManager)
	mockMgr.err = fmt.Errorf("%w: must be in the future", instances.ErrInvalidExpiresAt)
	svc.InstanceManager = mockMgr
	expiresAt := time.Now().Add(-time.Hour)

	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{Body: &oapi.CreateInstanceRequest{
		Name:      "test-expired-deadline",
		Image:     "docker.io/library/alpine:latest",
		ExpiresAt: &expiresAt,
	}})
	require.NoError(t, err)
	badRequest, ok := resp.(oapi.CreateInstance400JSONResponse)
	require.True(t, ok)
	assert.Equal(t, "invalid_expires_at", badRequest.Code)
}

func TestInstanceResponseIncludesNullExpiresAt(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(instanceToOAPI(instances.Instance{}))
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"expires_at":null`)
}

func TestCreateInstance_RejectsConflictingExpiration(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	ttl := "1h"
	expiresAt := time.Now().Add(2 * time.Hour)
	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{Body: &oapi.CreateInstanceRequest{
		Name:      "test-conflicting-expiration",
		Image:     "docker.io/library/alpine:latest",
		Ttl:       &ttl,
		ExpiresAt: &expiresAt,
	}})
	require.NoError(t, err)
	badRequest, ok := resp.(oapi.CreateInstance400JSONResponse)
	require.True(t, ok)
	assert.Equal(t, "invalid_request", badRequest.Code)
}

func TestCreateInstance_InvalidTTL(t *testing.T) {
	t.Parallel()

	for _, ttl := range []string{"not-a-duration", "-1h"} {
		t.Run(ttl, func(t *testing.T) {
			svc := newTestService(t)
			resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{Body: &oapi.CreateInstanceRequest{
				Name:  "test-invalid-ttl",
				Image: "docker.io/library/alpine:latest",
				Ttl:   &ttl,
			}})
			require.NoError(t, err)
			badRequest, ok := resp.(oapi.CreateInstance400JSONResponse)
			require.True(t, ok)
			assert.Equal(t, "invalid_ttl", badRequest.Code)
		})
	}
}

func TestCreateInstance_MapsQEMUMicroVMHypervisor(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	mockMgr := newCaptureCreateManager(svc.InstanceManager)
	svc.InstanceManager = mockMgr
	microvm := oapi.CreateInstanceRequestHypervisor(hypervisor.TypeQEMUMicroVM)

	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{Body: &oapi.CreateInstanceRequest{
		Name:       "test-qemu-microvm",
		Image:      "docker.io/library/alpine:latest",
		Hypervisor: &microvm,
	}})
	require.NoError(t, err)
	require.IsType(t, oapi.CreateInstance201JSONResponse{}, resp)
	require.NotNil(t, mockMgr.lastReq)
	assert.Equal(t, hypervisor.TypeQEMUMicroVM, mockMgr.lastReq.Hypervisor)
}

func TestInstanceToOAPI_QEMUMicroVMHypervisor(t *testing.T) {
	t.Parallel()

	inst := instances.Instance{StoredMetadata: instances.StoredMetadata{
		Id: "qemu-microvm", Name: "qemu-microvm", Image: "alpine", CreatedAt: time.Now(),
		HypervisorType: hypervisor.TypeQEMUMicroVM,
	}}
	oapiInst := instanceToOAPI(inst)
	require.NotNil(t, oapiInst.Hypervisor)
	assert.Equal(t, string(hypervisor.TypeQEMUMicroVM), string(*oapiInst.Hypervisor))
}

func TestCreateInstance_OmittedHotplugSizeDefaultsToZero(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	origMgr := svc.InstanceManager
	mockMgr := newCaptureCreateManager(origMgr)
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

func TestCreateInstance_MapsNetworkEgressCredentials(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	origMgr := svc.InstanceManager
	mockMgr := newCaptureCreateManager(origMgr)
	svc.InstanceManager = mockMgr

	networkEnabled := true
	egressEnabled := true
	credentials := map[string]oapi.CreateInstanceRequestCredential{
		"OUTBOUND_OPENAI_KEY": {
			Source: oapi.CreateInstanceRequestCredentialSource{
				Env: "OUTBOUND_OPENAI_KEY",
			},
			Inject: []oapi.CreateInstanceRequestCredentialInject{
				{
					Hosts: &[]string{"api.openai.com", "*.openai.com"},
					As: oapi.CreateInstanceRequestCredentialInjectAs{
						Header: "Authorization",
						Format: "Bearer ${value}",
					},
				},
			},
		},
		"GITHUB_TOKEN": {
			Source: oapi.CreateInstanceRequestCredentialSource{
				Env: "GITHUB_TOKEN",
			},
			Inject: []oapi.CreateInstanceRequestCredentialInject{
				{
					As: oapi.CreateInstanceRequestCredentialInjectAs{
						Header: "X-GitHub-Token",
						Format: "${value}",
					},
				},
			},
		},
	}
	env := map[string]string{
		"OUTBOUND_OPENAI_KEY": "real-openai-key-123",
		"GITHUB_TOKEN":        "real-gh-token-456",
	}

	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:  "test-egress-proxy-mock-env-vars",
			Image: "docker.io/library/alpine:latest",
			Env:   &env,
			Network: &struct {
				BandwidthDownload *string                                  `json:"bandwidth_download,omitempty"`
				BandwidthUpload   *string                                  `json:"bandwidth_upload,omitempty"`
				Egress            *oapi.CreateInstanceRequestNetworkEgress `json:"egress,omitempty"`
				Enabled           *bool                                    `json:"enabled,omitempty"`
			}{
				Enabled: &networkEnabled,
				Egress: &oapi.CreateInstanceRequestNetworkEgress{
					Enabled: &egressEnabled,
				},
			},
			Credentials: &credentials,
		},
	})
	require.NoError(t, err)
	_, ok := resp.(oapi.CreateInstance201JSONResponse)
	require.True(t, ok, "expected 201 response")

	require.NotNil(t, mockMgr.lastReq)
	require.NotNil(t, mockMgr.lastReq.NetworkEgress)
	assert.True(t, mockMgr.lastReq.NetworkEgress.Enabled)
	assert.Equal(t, instances.EgressEnforcementModeAll, mockMgr.lastReq.NetworkEgress.EnforcementMode)
	assert.Equal(t, "OUTBOUND_OPENAI_KEY", mockMgr.lastReq.Credentials["OUTBOUND_OPENAI_KEY"].Source.Env)
	assert.Equal(t, []string{"api.openai.com", "*.openai.com"}, mockMgr.lastReq.Credentials["OUTBOUND_OPENAI_KEY"].Inject[0].Hosts)
	assert.Equal(t, "Authorization", mockMgr.lastReq.Credentials["OUTBOUND_OPENAI_KEY"].Inject[0].As.Header)
	assert.Equal(t, "Bearer ${value}", mockMgr.lastReq.Credentials["OUTBOUND_OPENAI_KEY"].Inject[0].As.Format)
	assert.Equal(t, "real-openai-key-123", mockMgr.lastReq.Env["OUTBOUND_OPENAI_KEY"])
	assert.Equal(t, "real-gh-token-456", mockMgr.lastReq.Env["GITHUB_TOKEN"])
}

func TestCreateInstance_MapsNetworkEgressEnforcementMode(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	origMgr := svc.InstanceManager
	mockMgr := newCaptureCreateManager(origMgr)
	svc.InstanceManager = mockMgr

	networkEnabled := true
	egressEnabled := true
	mode := oapi.HttpHttpsOnly
	env := map[string]string{
		"OUTBOUND_OPENAI_KEY": "real-openai-key-123",
	}
	credentials := map[string]oapi.CreateInstanceRequestCredential{
		"OUTBOUND_OPENAI_KEY": {
			Source: oapi.CreateInstanceRequestCredentialSource{
				Env: "OUTBOUND_OPENAI_KEY",
			},
			Inject: []oapi.CreateInstanceRequestCredentialInject{
				{
					As: oapi.CreateInstanceRequestCredentialInjectAs{
						Header: "Authorization",
						Format: "Bearer ${value}",
					},
				},
			},
		},
	}

	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:  "test-egress-proxy-enforcement-mode",
			Image: "docker.io/library/alpine:latest",
			Env:   &env,
			Network: &struct {
				BandwidthDownload *string                                  `json:"bandwidth_download,omitempty"`
				BandwidthUpload   *string                                  `json:"bandwidth_upload,omitempty"`
				Egress            *oapi.CreateInstanceRequestNetworkEgress `json:"egress,omitempty"`
				Enabled           *bool                                    `json:"enabled,omitempty"`
			}{
				Enabled: &networkEnabled,
				Egress: &oapi.CreateInstanceRequestNetworkEgress{
					Enabled: &egressEnabled,
					Enforcement: &oapi.CreateInstanceRequestNetworkEgressEnforcement{
						Mode: &mode,
					},
				},
			},
			Credentials: &credentials,
		},
	})
	require.NoError(t, err)
	_, ok := resp.(oapi.CreateInstance201JSONResponse)
	require.True(t, ok, "expected 201 response")

	require.NotNil(t, mockMgr.lastReq)
	require.NotNil(t, mockMgr.lastReq.NetworkEgress)
	assert.Equal(t, instances.EgressEnforcementModeHTTPHTTPSOnly, mockMgr.lastReq.NetworkEgress.EnforcementMode)
}

func TestCreateInstance_MapsStandbyCompressionDelayInSnapshotPolicy(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	origMgr := svc.InstanceManager
	mockMgr := newCaptureCreateManager(origMgr)
	svc.InstanceManager = mockMgr

	delay := "2m30s"
	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:  "test-standby-compression-delay",
			Image: "docker.io/library/alpine:latest",
			SnapshotPolicy: &oapi.SnapshotPolicy{
				StandbyCompressionDelay: &delay,
			},
		},
	})
	require.NoError(t, err)
	_, ok := resp.(oapi.CreateInstance201JSONResponse)
	require.True(t, ok, "expected 201 response")

	require.NotNil(t, mockMgr.lastReq)
	require.NotNil(t, mockMgr.lastReq.SnapshotPolicy)
	require.NotNil(t, mockMgr.lastReq.SnapshotPolicy.StandbyCompressionDelay)
	assert.Equal(t, 150*time.Second, *mockMgr.lastReq.SnapshotPolicy.StandbyCompressionDelay)
}

func TestCreateInstance_InvalidStandbyCompressionDelayInSnapshotPolicy(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	delay := "not-a-duration"

	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:  "test-invalid-standby-delay",
			Image: "docker.io/library/alpine:latest",
			SnapshotPolicy: &oapi.SnapshotPolicy{
				StandbyCompressionDelay: &delay,
			},
		},
	})
	require.NoError(t, err)

	badReq, ok := resp.(oapi.CreateInstance400JSONResponse)
	require.True(t, ok, "expected 400 response")
	assert.Equal(t, "invalid_snapshot_policy", badReq.Code)
	assert.Contains(t, badReq.Message, "standby_compression_delay")
}

func TestInstanceToOAPI_EmitsPhaseAccounting(t *testing.T) {
	t.Parallel()

	t0 := time.Now().Add(-10 * time.Minute)
	tr := phasetracking.Tracker{}
	tr.Record(phasetracking.PhaseRunning, t0)
	tr.Record(phasetracking.PhaseStandby, t0.Add(60*time.Second))
	tr.Record(phasetracking.PhaseRunning, t0.Add(60*time.Second+5*time.Minute))

	inst := instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-phases",
			Name:           "inst-phases",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      t0,
			HypervisorType: hypervisor.TypeCloudHypervisor,
			Phases:         tr,
		},
		State: instances.StateRunning,
	}

	oapiInst := instanceToOAPI(inst)

	require.NotNil(t, oapiInst.CurrentPhase)
	assert.Equal(t, "running", *oapiInst.CurrentPhase)
	require.NotNil(t, oapiInst.CurrentPhaseSince)
	require.NotNil(t, oapiInst.PhaseDurationsMs)

	durations := *oapiInst.PhaseDurationsMs
	// Standby stint was a completed 300s window — no live accrual since.
	assert.Equal(t, int64(300_000), durations["standby"])
	// Running = 60s completed + live time since latest Record. The
	// recorded-at instant is in the past, so this must be >= 60s.
	assert.GreaterOrEqual(t, durations["running"], int64(60_000),
		"running should include the completed 60s stint")
}

func TestInstanceToOAPI_OmitsPhaseFieldsWhenUnset(t *testing.T) {
	t.Parallel()

	inst := instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-no-phases",
			Name:           "inst-no-phases",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      time.Now(),
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateStopped,
	}

	oapiInst := instanceToOAPI(inst)

	assert.Nil(t, oapiInst.CurrentPhase)
	assert.Nil(t, oapiInst.CurrentPhaseSince)
	assert.Nil(t, oapiInst.PhaseDurationsMs)
}

func TestInstanceToOAPI_EchoesResolvedPlatform(t *testing.T) {
	t.Parallel()

	inst := instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-platform",
			Name:           "inst-platform",
			Image:          "docker.io/library/alpine@sha256:deadbeef",
			Platform:       "linux/amd64",
			CreatedAt:      time.Now(),
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateRunning,
	}

	oapiInst := instanceToOAPI(inst)
	require.NotNil(t, oapiInst.Platform)
	assert.Equal(t, "linux/amd64", *oapiInst.Platform)
}

func TestInstanceToOAPI_OmitsPlatformWhenUnset(t *testing.T) {
	t.Parallel()

	inst := instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-no-platform",
			Name:           "inst-no-platform",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      time.Now(),
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateStopped,
	}

	oapiInst := instanceToOAPI(inst)
	assert.Nil(t, oapiInst.Platform)
}

// errCreateInstanceManager is a fake whose CreateInstance always fails with a
// preset error, used to assert the handler maps typed errors to statuses.
type errCreateInstanceManager struct {
	instances.Manager
	err error
}

func (m *errCreateInstanceManager) CreateInstance(context.Context, instances.CreateInstanceRequest) (*instances.Instance, error) {
	return nil, m.err
}

func TestCreateInstance_ErrorStatusMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		err         error
		wantType    any
		wantCode    string
		wantMessage string
	}{
		{
			name:     "platform not available -> 404",
			err:      fmt.Errorf("resolve image: %w", images.ErrPlatformNotAvailable),
			wantType: oapi.CreateInstance404JSONResponse{},
			wantCode: "platform_not_available",
		},
		{
			name:     "rate limited -> 429",
			err:      fmt.Errorf("resolve image: %w", images.ErrRateLimited),
			wantType: oapi.CreateInstance429JSONResponse{},
			wantCode: "rate_limited",
		},
		{
			name:     "image not found -> 404",
			err:      fmt.Errorf("resolve image: %w", images.ErrNotFound),
			wantType: oapi.CreateInstance404JSONResponse{},
			wantCode: "not_found",
		},
		{
			name:     "invalid platform -> 400",
			err:      fmt.Errorf("resolve image: %w", images.ErrInvalidPlatform),
			wantType: oapi.CreateInstance400JSONResponse{},
			wantCode: "invalid_platform",
		},
		{
			name:        "macOS vGPU unsupported -> 400",
			err:         fmt.Errorf("%w: %w", instances.ErrInvalidRequest, devices.ErrVGPUNotSupportedOnMacOS),
			wantType:    oapi.CreateInstance400JSONResponse{},
			wantCode:    "invalid_request",
			wantMessage: "invalid request: vGPU (mdev) is not supported on macOS",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := newTestService(t)
			svc.InstanceManager = &errCreateInstanceManager{Manager: svc.InstanceManager, err: tc.err}

			resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
				Body: &oapi.CreateInstanceRequest{
					Name:  "inst-err",
					Image: "docker.io/library/alpine:3.19",
				},
			})
			require.NoError(t, err)
			require.IsType(t, tc.wantType, resp)
			require.Equal(t, tc.wantCode, createInstanceErrorCodeOf(resp))
			if tc.wantMessage != "" {
				response := resp.(oapi.CreateInstance400JSONResponse)
				require.Equal(t, tc.wantMessage, response.Message)
			}
		})
	}
}

// errActionInstanceManager is a fake whose lifecycle actions always fail with a
// preset error.
type errActionInstanceManager struct {
	instances.Manager
	err error
}

func (m *errActionInstanceManager) StartInstance(context.Context, string, instances.StartInstanceRequest) (*instances.Instance, error) {
	return nil, m.err
}

func (m *errActionInstanceManager) RestoreInstance(context.Context, string) (*instances.Instance, error) {
	return nil, m.err
}

func (m *errActionInstanceManager) ForkInstance(context.Context, string, instances.ForkInstanceRequest) (*instances.Instance, error) {
	return nil, m.err
}

func (m *errActionInstanceManager) RestoreSnapshot(context.Context, string, string, instances.RestoreSnapshotRequest) (*instances.Instance, error) {
	return nil, m.err
}

func TestRestoreInstance_ErrorMapping(t *testing.T) {
	t.Parallel()

	resolved := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{Id: "inst-restore-error"},
	}
	tests := []struct {
		name         string
		err          error
		wantResponse oapi.RestoreInstanceResponseObject
	}{
		{
			name: "client cancellation -> 499",
			err:  fmt.Errorf("restore from snapshot: %w", context.Canceled),
			wantResponse: oapi.RestoreInstance499JSONResponse{
				Code:    "client_closed_request",
				Message: "request canceled",
			},
		},
		{
			name: "internal failure -> 500",
			err:  fmt.Errorf("restore from snapshot: hypervisor failed"),
			wantResponse: oapi.RestoreInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to restore instance",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc := newTestService(t)
			svc.InstanceManager = &errActionInstanceManager{Manager: svc.InstanceManager, err: tt.err}

			resp, err := svc.RestoreInstance(
				mw.WithResolvedInstance(ctx(), resolved.Id, resolved),
				oapi.RestoreInstanceRequestObject{Id: resolved.Id},
			)
			require.NoError(t, err)
			require.Equal(t, tt.wantResponse, resp)
		})
	}
}

func TestInstanceActions_ImageNotFoundMapsTo404(t *testing.T) {
	t.Parallel()

	// The start path wraps the image lookup with %w, so a deleted image surfaces
	// as images.ErrNotFound through the manager.
	err := fmt.Errorf("get image: %w", images.ErrNotFound)
	resolved := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-img-gone",
			Name:           "inst-img-gone",
			Image:          "docker.io/library/alpine:3.19",
			CreatedAt:      time.Now(),
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateStopped,
	}

	t.Run("start -> 404", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t)
		svc.InstanceManager = &errActionInstanceManager{Manager: svc.InstanceManager, err: err}
		resp, rerr := svc.StartInstance(mw.WithResolvedInstance(ctx(), resolved.Id, resolved), oapi.StartInstanceRequestObject{Id: resolved.Id})
		require.NoError(t, rerr)
		r, ok := resp.(oapi.StartInstance404JSONResponse)
		require.True(t, ok, "expected 404 response, got %T", resp)
		require.Equal(t, "not_found", r.Code)
	})

	t.Run("restore -> 404", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t)
		svc.InstanceManager = &errActionInstanceManager{Manager: svc.InstanceManager, err: err}
		resp, rerr := svc.RestoreInstance(mw.WithResolvedInstance(ctx(), resolved.Id, resolved), oapi.RestoreInstanceRequestObject{Id: resolved.Id})
		require.NoError(t, rerr)
		r, ok := resp.(oapi.RestoreInstance404JSONResponse)
		require.True(t, ok, "expected 404 response, got %T", resp)
		require.Equal(t, "not_found", r.Code)
	})

	t.Run("fork -> 404", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t)
		svc.InstanceManager = &errActionInstanceManager{Manager: svc.InstanceManager, err: err}
		forkName := "inst-img-gone-fork"
		resp, rerr := svc.ForkInstance(mw.WithResolvedInstance(ctx(), resolved.Id, resolved), oapi.ForkInstanceRequestObject{
			Id:   resolved.Id,
			Body: &oapi.ForkInstanceRequest{Name: forkName},
		})
		require.NoError(t, rerr)
		r, ok := resp.(oapi.ForkInstance404JSONResponse)
		require.True(t, ok, "expected 404 response, got %T", resp)
		require.Equal(t, "not_found", r.Code)
	})

	t.Run("restore snapshot -> 404", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t)
		svc.InstanceManager = &errActionInstanceManager{Manager: svc.InstanceManager, err: err}
		resp, rerr := svc.RestoreInstanceSnapshot(mw.WithResolvedInstance(ctx(), resolved.Id, resolved), oapi.RestoreInstanceSnapshotRequestObject{
			Id:         resolved.Id,
			SnapshotId: "snap-1",
			Body:       &oapi.RestoreInstanceSnapshotJSONRequestBody{},
		})
		require.NoError(t, rerr)
		r, ok := resp.(oapi.RestoreInstanceSnapshot404JSONResponse)
		require.True(t, ok, "expected 404 response, got %T", resp)
		require.Equal(t, "not_found", r.Code)
	})
}

// createInstanceErrorCodeOf extracts the Code field from a CreateInstance error response.
func createInstanceErrorCodeOf(resp oapi.CreateInstanceResponseObject) string {
	switch r := resp.(type) {
	case oapi.CreateInstance400JSONResponse:
		return r.Code
	case oapi.CreateInstance404JSONResponse:
		return r.Code
	case oapi.CreateInstance409JSONResponse:
		return r.Code
	case oapi.CreateInstance429JSONResponse:
		return r.Code
	case oapi.CreateInstance500JSONResponse:
		return r.Code
	default:
		return ""
	}
}

func TestInstanceToOAPI_EmitsStandbyCompressionDelayInSnapshotPolicy(t *testing.T) {
	t.Parallel()

	delay := 90 * time.Second
	inst := instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-standby-delay",
			Name:           "inst-standby-delay",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      time.Now(),
			HypervisorType: hypervisor.TypeCloudHypervisor,
			SnapshotPolicy: &instances.SnapshotPolicy{
				StandbyCompressionDelay: &delay,
			},
		},
		State: instances.StateStandby,
	}

	oapiInst := instanceToOAPI(inst)
	require.NotNil(t, oapiInst.SnapshotPolicy)
	require.NotNil(t, oapiInst.SnapshotPolicy.StandbyCompressionDelay)
	assert.Equal(t, "1m30s", *oapiInst.SnapshotPolicy.StandbyCompressionDelay)
}

func TestCreateInstance_MapsAutoStandbyPolicy(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	origMgr := svc.InstanceManager
	mockMgr := newCaptureCreateManager(origMgr)
	svc.InstanceManager = mockMgr

	enabled := true
	idleTimeout := "5m"
	ignoreSourceCidrs := []string{"10.0.0.0/8", "192.168.0.0/16"}
	ignoreDestinationPorts := []int{22, 9000}

	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:  "test-auto-standby",
			Image: "docker.io/library/alpine:latest",
			AutoStandby: &oapi.AutoStandbyPolicy{
				Enabled:                &enabled,
				IdleTimeout:            &idleTimeout,
				IgnoreSourceCidrs:      &ignoreSourceCidrs,
				IgnoreDestinationPorts: &ignoreDestinationPorts,
			},
		},
	})
	require.NoError(t, err)

	created, ok := resp.(oapi.CreateInstance201JSONResponse)
	require.True(t, ok, "expected 201 response")
	require.NotNil(t, mockMgr.lastReq)
	require.NotNil(t, mockMgr.lastReq.AutoStandby)
	assert.True(t, mockMgr.lastReq.AutoStandby.Enabled)
	assert.Equal(t, "5m", mockMgr.lastReq.AutoStandby.IdleTimeout)
	assert.Equal(t, []string{"10.0.0.0/8", "192.168.0.0/16"}, mockMgr.lastReq.AutoStandby.IgnoreSourceCIDRs)
	assert.Equal(t, []uint16{22, 9000}, mockMgr.lastReq.AutoStandby.IgnoreDestinationPorts)

	instance := oapi.Instance(created)
	require.NotNil(t, instance.AutoStandby)
	require.NotNil(t, instance.AutoStandby.Enabled)
	assert.True(t, *instance.AutoStandby.Enabled)
	assert.Equal(t, idleTimeout, *instance.AutoStandby.IdleTimeout)
}

func TestCreateInstance_MapsHealthCheckPolicy(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	origMgr := svc.InstanceManager
	mockMgr := newCaptureCreateManager(origMgr)
	svc.InstanceManager = mockMgr

	typ := oapi.HealthCheckTypeExec
	interval := "5s"
	timeout := "1s"

	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:  "test-health-check",
			Image: "docker.io/library/alpine:latest",
			HealthCheck: &oapi.HealthCheck{
				Type:     &typ,
				Interval: &interval,
				Timeout:  &timeout,
				Exec: &oapi.HealthCheckExec{
					Command: []string{"true"},
				},
			},
		},
	})
	require.NoError(t, err)

	created, ok := resp.(oapi.CreateInstance201JSONResponse)
	require.True(t, ok, "expected 201 response")
	require.NotNil(t, mockMgr.lastReq)
	require.NotNil(t, mockMgr.lastReq.HealthCheck)
	assert.Equal(t, healthcheck.TypeExec, mockMgr.lastReq.HealthCheck.Type)
	assert.Equal(t, []string{"true"}, mockMgr.lastReq.HealthCheck.Exec.Command)
	assert.Equal(t, "5s", mockMgr.lastReq.HealthCheck.Interval)
	assert.Equal(t, "1s", mockMgr.lastReq.HealthCheck.Timeout)

	instance := oapi.Instance(created)
	require.NotNil(t, instance.HealthCheck)
	require.NotNil(t, instance.HealthCheck.Type)
	assert.Equal(t, oapi.HealthCheckTypeExec, *instance.HealthCheck.Type)
	require.NotNil(t, instance.HealthStatus)
	assert.Equal(t, oapi.InstanceHealthStatusStatusStarting, instance.HealthStatus.Status)
}

func TestCreateInstance_MapsRestartPolicy(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	origMgr := svc.InstanceManager
	mockMgr := newCaptureCreateManager(origMgr)
	svc.InstanceManager = mockMgr

	policy := oapi.OnFailure
	backoff := "7s"
	stableAfter := "2m"
	maxAttempts := 4

	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:  "test-restart-policy",
			Image: "docker.io/library/alpine:latest",
			RestartPolicy: &oapi.RestartPolicy{
				Policy:      &policy,
				Backoff:     &backoff,
				StableAfter: &stableAfter,
				MaxAttempts: &maxAttempts,
			},
		},
	})
	require.NoError(t, err)

	created, ok := resp.(oapi.CreateInstance201JSONResponse)
	require.True(t, ok, "expected 201 response")
	require.NotNil(t, mockMgr.lastReq)
	require.NotNil(t, mockMgr.lastReq.RestartPolicy)
	assert.Equal(t, restartpolicy.PolicyOnFailure, mockMgr.lastReq.RestartPolicy.Policy)
	assert.Equal(t, "7s", mockMgr.lastReq.RestartPolicy.Backoff)
	assert.Equal(t, "2m0s", mockMgr.lastReq.RestartPolicy.StableAfter)
	assert.Equal(t, 4, mockMgr.lastReq.RestartPolicy.MaxAttempts)

	instance := oapi.Instance(created)
	require.NotNil(t, instance.RestartPolicy)
	require.NotNil(t, instance.RestartPolicy.Policy)
	assert.Equal(t, oapi.OnFailure, *instance.RestartPolicy.Policy)
}

func TestUpdateInstance_MapsEnvPatch(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	origMgr := svc.InstanceManager
	now := time.Now()
	result := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-update",
			Name:           "inst-update",
			Image:          "docker.io/library/alpine:latest",
			Env:            map[string]string{"OUTBOUND_OPENAI_KEY": "rotated-key-456"},
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateRunning,
	}
	mockMgr := newCaptureUpdateManager(origMgr)
	mockMgr.result = result
	svc.InstanceManager = mockMgr

	env := map[string]string{"OUTBOUND_OPENAI_KEY": "rotated-key-456"}
	resolved := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-update",
			Name:           "inst-update",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateRunning,
	}

	resp, err := svc.UpdateInstance(mw.WithResolvedInstance(ctx(), resolved.Id, resolved), oapi.UpdateInstanceRequestObject{
		Id:   resolved.Id,
		Body: &oapi.UpdateInstanceRequest{Env: &env},
	})
	require.NoError(t, err)
	_, ok := resp.(oapi.UpdateInstance200JSONResponse)
	require.True(t, ok, "expected 200 response")

	require.NotNil(t, mockMgr.lastReq)
	assert.Equal(t, resolved.Id, mockMgr.lastID)
	assert.Equal(t, "rotated-key-456", mockMgr.lastReq.Env["OUTBOUND_OPENAI_KEY"])
}

func TestUpdateInstance_MapsTTL(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	mockMgr := newCaptureUpdateManager(svc.InstanceManager)
	svc.InstanceManager = mockMgr
	resolved := &instances.Instance{StoredMetadata: instances.StoredMetadata{Id: "inst-update-ttl"}}
	ttl := "6h"

	resp, err := svc.UpdateInstance(mw.WithResolvedInstance(ctx(), resolved.Id, resolved), oapi.UpdateInstanceRequestObject{
		Id:   resolved.Id,
		Body: &oapi.UpdateInstanceRequest{Ttl: &ttl},
	})
	require.NoError(t, err)
	updated, ok := resp.(oapi.UpdateInstance200JSONResponse)
	require.True(t, ok)
	require.NotNil(t, mockMgr.lastReq.TTL)
	assert.Equal(t, 6*time.Hour, *mockMgr.lastReq.TTL)
	require.NotNil(t, updated.ExpiresAt)
	assert.WithinDuration(t, time.Now().Add(6*time.Hour), *updated.ExpiresAt, time.Second)
}

func TestUpdateInstance_ZeroTTLDisablesExpiration(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	mockMgr := newCaptureUpdateManager(svc.InstanceManager)
	svc.InstanceManager = mockMgr
	resolved := &instances.Instance{StoredMetadata: instances.StoredMetadata{Id: "inst-disable-expiration"}}
	ttl := "0s"

	resp, err := svc.UpdateInstance(mw.WithResolvedInstance(ctx(), resolved.Id, resolved), oapi.UpdateInstanceRequestObject{
		Id:   resolved.Id,
		Body: &oapi.UpdateInstanceRequest{Ttl: &ttl},
	})
	require.NoError(t, err)
	updated, ok := resp.(oapi.UpdateInstance200JSONResponse)
	require.True(t, ok)
	require.NotNil(t, mockMgr.lastReq.TTL)
	assert.Zero(t, *mockMgr.lastReq.TTL)
	assert.Nil(t, updated.ExpiresAt)
}

func TestUpdateInstance_MapsExpiresAt(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	mockMgr := newCaptureUpdateManager(svc.InstanceManager)
	svc.InstanceManager = mockMgr
	resolved := &instances.Instance{StoredMetadata: instances.StoredMetadata{Id: "inst-update-expires-at"}}
	expiresAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	resp, err := svc.UpdateInstance(mw.WithResolvedInstance(ctx(), resolved.Id, resolved), oapi.UpdateInstanceRequestObject{
		Id:   resolved.Id,
		Body: &oapi.UpdateInstanceRequest{ExpiresAt: &expiresAt},
	})
	require.NoError(t, err)
	updated, ok := resp.(oapi.UpdateInstance200JSONResponse)
	require.True(t, ok)
	require.NotNil(t, mockMgr.lastReq.ExpiresAt)
	assert.Equal(t, expiresAt, *mockMgr.lastReq.ExpiresAt)
	require.NotNil(t, updated.ExpiresAt)
	assert.Equal(t, expiresAt, *updated.ExpiresAt)
}

func TestUpdateInstance_RejectsConflictingExpiration(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	resolved := &instances.Instance{StoredMetadata: instances.StoredMetadata{Id: "inst-conflicting-expiration"}}
	ttl := "1h"
	expiresAt := time.Now().Add(2 * time.Hour)

	resp, err := svc.UpdateInstance(mw.WithResolvedInstance(ctx(), resolved.Id, resolved), oapi.UpdateInstanceRequestObject{
		Id: resolved.Id,
		Body: &oapi.UpdateInstanceRequest{
			Ttl:       &ttl,
			ExpiresAt: &expiresAt,
		},
	})
	require.NoError(t, err)
	badRequest, ok := resp.(oapi.UpdateInstance400JSONResponse)
	require.True(t, ok)
	assert.Equal(t, "invalid_request", badRequest.Code)
}

func TestUpdateInstance_MapsAutoStandbyPatch(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	origMgr := svc.InstanceManager
	now := time.Now()
	result := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-update-auto-standby",
			Name:           "inst-update-auto-standby",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
			AutoStandby: &autostandby.Policy{
				Enabled:     true,
				IdleTimeout: "10m0s",
			},
		},
		State: instances.StateStopped,
	}
	mockMgr := newCaptureUpdateManager(origMgr)
	mockMgr.result = result
	svc.InstanceManager = mockMgr

	enabled := true
	idleTimeout := "10m"
	ignoreDestinationPorts := []int{22}
	resolved := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-update-auto-standby",
			Name:           "inst-update-auto-standby",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateStopped,
	}

	resp, err := svc.UpdateInstance(mw.WithResolvedInstance(ctx(), resolved.Id, resolved), oapi.UpdateInstanceRequestObject{
		Id: resolved.Id,
		Body: &oapi.UpdateInstanceRequest{
			AutoStandby: &oapi.AutoStandbyPolicy{
				Enabled:                &enabled,
				IdleTimeout:            &idleTimeout,
				IgnoreDestinationPorts: &ignoreDestinationPorts,
			},
		},
	})
	require.NoError(t, err)
	updated, ok := resp.(oapi.UpdateInstance200JSONResponse)
	require.True(t, ok, "expected 200 response")

	require.NotNil(t, mockMgr.lastReq)
	require.NotNil(t, mockMgr.lastReq.AutoStandby)
	assert.Equal(t, resolved.Id, mockMgr.lastID)
	assert.True(t, mockMgr.lastReq.AutoStandby.Enabled)
	assert.Equal(t, "10m", mockMgr.lastReq.AutoStandby.IdleTimeout)
	assert.Equal(t, []uint16{22}, mockMgr.lastReq.AutoStandby.IgnoreDestinationPorts)

	instance := oapi.Instance(updated)
	require.NotNil(t, instance.AutoStandby)
	require.NotNil(t, instance.AutoStandby.Enabled)
	assert.True(t, *instance.AutoStandby.Enabled)
}

func TestUpdateInstance_MapsHealthCheckPatch(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	origMgr := svc.InstanceManager
	now := time.Now()
	result := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-update-health-check",
			Name:           "inst-update-health-check",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
			HealthCheck: &healthcheck.Policy{
				Type: healthcheck.TypeTCP,
				TCP:  &healthcheck.TCPCheck{Port: 8080},
			},
		},
		State: instances.StateStopped,
	}
	mockMgr := newCaptureUpdateManager(origMgr)
	mockMgr.result = result
	svc.InstanceManager = mockMgr

	typ := oapi.HealthCheckTypeTcp
	resolved := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-update-health-check",
			Name:           "inst-update-health-check",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
			NetworkEnabled: true,
		},
		State: instances.StateStopped,
	}

	resp, err := svc.UpdateInstance(mw.WithResolvedInstance(ctx(), resolved.Id, resolved), oapi.UpdateInstanceRequestObject{
		Id: resolved.Id,
		Body: &oapi.UpdateInstanceRequest{
			HealthCheck: &oapi.HealthCheck{
				Type: &typ,
				Tcp:  &oapi.HealthCheckTCP{Port: 8080},
			},
		},
	})
	require.NoError(t, err)
	updated, ok := resp.(oapi.UpdateInstance200JSONResponse)
	require.True(t, ok, "expected 200 response")

	require.NotNil(t, mockMgr.lastReq)
	require.NotNil(t, mockMgr.lastReq.HealthCheck)
	assert.Equal(t, resolved.Id, mockMgr.lastID)
	assert.Equal(t, healthcheck.TypeTCP, mockMgr.lastReq.HealthCheck.Type)
	assert.Equal(t, uint16(8080), mockMgr.lastReq.HealthCheck.TCP.Port)

	instance := oapi.Instance(updated)
	require.NotNil(t, instance.HealthCheck)
	require.NotNil(t, instance.HealthCheck.Type)
	assert.Equal(t, oapi.HealthCheckTypeTcp, *instance.HealthCheck.Type)
	require.NotNil(t, instance.HealthStatus)
	assert.Equal(t, oapi.InstanceHealthStatusStatusUnknown, instance.HealthStatus.Status)
}

func TestUpdateInstance_MapsRestartPolicyPatch(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	origMgr := svc.InstanceManager
	now := time.Now()
	result := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-update-restart-policy",
			Name:           "inst-update-restart-policy",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
			RestartPolicy: &restartpolicy.Policy{
				Policy:      restartpolicy.PolicyAlways,
				Backoff:     "5s",
				StableAfter: "10m0s",
			},
			RestartStatus: restartpolicy.Status{
				BlockedReason: restartpolicy.BlockedReasonManualStop,
			},
		},
		State: instances.StateStopped,
	}
	mockMgr := newCaptureUpdateManager(origMgr)
	mockMgr.result = result
	svc.InstanceManager = mockMgr

	policy := oapi.Always
	resolved := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-update-restart-policy",
			Name:           "inst-update-restart-policy",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateStopped,
	}

	resp, err := svc.UpdateInstance(mw.WithResolvedInstance(ctx(), resolved.Id, resolved), oapi.UpdateInstanceRequestObject{
		Id: resolved.Id,
		Body: &oapi.UpdateInstanceRequest{
			RestartPolicy: &oapi.RestartPolicy{Policy: &policy},
		},
	})
	require.NoError(t, err)
	updated, ok := resp.(oapi.UpdateInstance200JSONResponse)
	require.True(t, ok, "expected 200 response")

	require.NotNil(t, mockMgr.lastReq)
	assert.True(t, mockMgr.lastReq.RestartPolicySet)
	require.NotNil(t, mockMgr.lastReq.RestartPolicy)
	assert.Equal(t, restartpolicy.PolicyAlways, mockMgr.lastReq.RestartPolicy.Policy)

	instance := oapi.Instance(updated)
	require.NotNil(t, instance.RestartPolicy)
	require.NotNil(t, instance.RestartStatus)
	require.NotNil(t, instance.RestartStatus.BlockedReason)
	assert.Equal(t, oapi.ManualStop, *instance.RestartStatus.BlockedReason)
}

func TestUpdateInstance_RejectsInvalidRestartPolicy(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	origMgr := svc.InstanceManager
	mockMgr := newCaptureUpdateManager(origMgr)
	svc.InstanceManager = mockMgr

	now := time.Now()
	resolved := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-update-restart-policy",
			Name:           "inst-update-restart-policy",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateStopped,
	}
	policy := oapi.OnFailure
	backoff := "0s"

	resp, err := svc.UpdateInstance(mw.WithResolvedInstance(ctx(), resolved.Id, resolved), oapi.UpdateInstanceRequestObject{
		Id: resolved.Id,
		Body: &oapi.UpdateInstanceRequest{
			RestartPolicy: &oapi.RestartPolicy{
				Policy:  &policy,
				Backoff: &backoff,
			},
		},
	})
	require.NoError(t, err)

	badReq, ok := resp.(oapi.UpdateInstance400JSONResponse)
	require.True(t, ok, "expected 400 response")
	assert.Equal(t, "invalid_restart_policy", badReq.Code)
	assert.Nil(t, mockMgr.lastReq)
}

func TestUpdateInstance_RejectsZeroAutoStandbyIgnoreDestinationPort(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	origMgr := svc.InstanceManager
	now := time.Now()
	mockMgr := newCaptureUpdateManager(origMgr)
	svc.InstanceManager = mockMgr

	resolved := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-update-auto-standby",
			Name:           "inst-update-auto-standby",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateStopped,
	}
	enabled := true
	idleTimeout := "10m"
	ignoreDestinationPorts := []int{0}

	resp, err := svc.UpdateInstance(mw.WithResolvedInstance(ctx(), resolved.Id, resolved), oapi.UpdateInstanceRequestObject{
		Id: resolved.Id,
		Body: &oapi.UpdateInstanceRequest{
			AutoStandby: &oapi.AutoStandbyPolicy{
				Enabled:                &enabled,
				IdleTimeout:            &idleTimeout,
				IgnoreDestinationPorts: &ignoreDestinationPorts,
			},
		},
	})
	require.NoError(t, err)

	badReq, ok := resp.(oapi.UpdateInstance400JSONResponse)
	require.True(t, ok, "expected 400 response")
	assert.Equal(t, "invalid_auto_standby", badReq.Code)
	assert.Contains(t, badReq.Message, "between 1 and 65535")
	assert.Nil(t, mockMgr.lastReq)
}

func TestUpdateInstance_RequiresBody(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	now := time.Now()
	resolved := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-update",
			Name:           "inst-update",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateRunning,
	}

	resp, err := svc.UpdateInstance(mw.WithResolvedInstance(ctx(), resolved.Id, resolved), oapi.UpdateInstanceRequestObject{
		Id: resolved.Id,
	})
	require.NoError(t, err)
	badReq, ok := resp.(oapi.UpdateInstance400JSONResponse)
	require.True(t, ok, "expected 400 response")
	assert.Equal(t, "invalid_request", badReq.Code)
	assert.Contains(t, badReq.Message, "request body is required")
}

func TestUpdateInstance_MapsInvalidExpiresAt(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	mockMgr := newCaptureUpdateManager(svc.InstanceManager)
	mockMgr.err = fmt.Errorf("%w: must be in the future", instances.ErrInvalidExpiresAt)
	svc.InstanceManager = mockMgr
	resolved := &instances.Instance{StoredMetadata: instances.StoredMetadata{Id: "inst-invalid-expires-at"}}
	expiresAt := time.Now().Add(-time.Hour)

	resp, err := svc.UpdateInstance(mw.WithResolvedInstance(ctx(), resolved.Id, resolved), oapi.UpdateInstanceRequestObject{
		Id:   resolved.Id,
		Body: &oapi.UpdateInstanceRequest{ExpiresAt: &expiresAt},
	})
	require.NoError(t, err)
	badRequest, ok := resp.(oapi.UpdateInstance400JSONResponse)
	require.True(t, ok)
	assert.Equal(t, "invalid_expires_at", badRequest.Code)
}

func TestUpdateInstance_MapsExpiredError(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	mockMgr := newCaptureUpdateManager(svc.InstanceManager)
	mockMgr.err = fmt.Errorf("%w: expiration deadline has passed", instances.ErrInstanceExpired)
	svc.InstanceManager = mockMgr
	resolved := &instances.Instance{StoredMetadata: instances.StoredMetadata{Id: "inst-expired"}}
	ttl := "1h"

	resp, err := svc.UpdateInstance(mw.WithResolvedInstance(ctx(), resolved.Id, resolved), oapi.UpdateInstanceRequestObject{
		Id:   resolved.Id,
		Body: &oapi.UpdateInstanceRequest{Ttl: &ttl},
	})
	require.NoError(t, err)
	conflict, ok := resp.(oapi.UpdateInstance409JSONResponse)
	require.True(t, ok)
	assert.Equal(t, "instance_expired", conflict.Code)
}

func TestUpdateInstance_MapsInvalidRequestError(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	origMgr := svc.InstanceManager
	mockMgr := newCaptureUpdateManager(origMgr)
	mockMgr.err = fmt.Errorf("%w: env keys [UNRELATED_KEY] are not credential source env vars; allowed keys: [OUTBOUND_OPENAI_KEY]", instances.ErrInvalidRequest)
	svc.InstanceManager = mockMgr

	now := time.Now()
	resolved := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-update",
			Name:           "inst-update",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateRunning,
	}
	env := map[string]string{"UNRELATED_KEY": "value"}

	resp, err := svc.UpdateInstance(mw.WithResolvedInstance(ctx(), resolved.Id, resolved), oapi.UpdateInstanceRequestObject{
		Id:   resolved.Id,
		Body: &oapi.UpdateInstanceRequest{Env: &env},
	})
	require.NoError(t, err)
	badReq, ok := resp.(oapi.UpdateInstance400JSONResponse)
	require.True(t, ok, "expected 400 response")
	assert.Equal(t, "invalid_request", badReq.Code)
	assert.Contains(t, badReq.Message, "UNRELATED_KEY")
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

	mockMgr := newCaptureForkManager(svc.InstanceManager)
	mockMgr.result = forked
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

	mockMgr := newCaptureForkManager(svc.InstanceManager)
	mockMgr.err = instances.ErrNotSupported
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

	mockMgr := newCaptureForkManager(svc.InstanceManager)
	mockMgr.err = fmt.Errorf("%w: name is required", instances.ErrInvalidRequest)
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

func TestForkInstance_InsufficientResources(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	source := instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "src-instance",
			Name:           "src-instance",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      time.Now(),
			HypervisorType: hypervisor.TypeFirecracker,
		},
		State: instances.StateStandby,
	}

	mockMgr := newCaptureForkManager(svc.InstanceManager)
	mockMgr.err = fmt.Errorf("apply fork target state: %w: insufficient network bandwidth", instances.ErrInsufficientResources)
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

	conflict, ok := resp.(oapi.ForkInstance409JSONResponse)
	require.True(t, ok, "expected 409 response")
	assert.Equal(t, "insufficient_resources", conflict.Code)
}

func TestStandbyInstance_InvalidRequest(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	source := instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "standby-src",
			Name:           "standby-src",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      time.Now(),
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateStopped,
	}

	mockMgr := newCaptureStandbyManager(svc.InstanceManager)
	mockMgr.err = fmt.Errorf("%w: invalid snapshot compression level", instances.ErrInvalidRequest)
	svc.InstanceManager = mockMgr

	resp, err := svc.StandbyInstance(
		mw.WithResolvedInstance(ctx(), source.Id, source),
		oapi.StandbyInstanceRequestObject{
			Id: source.Id,
			Body: &oapi.StandbyInstanceRequest{
				Compression: &oapi.SnapshotCompressionConfig{
					Enabled: true,
				},
			},
		},
	)
	require.NoError(t, err)

	badReq, ok := resp.(oapi.StandbyInstance400JSONResponse)
	require.True(t, ok, "expected 400 response")
	assert.Equal(t, "invalid_request", badReq.Code)
	assert.Contains(t, badReq.Message, "invalid snapshot compression level")
}

func TestStandbyInstance_MapsCompressionDelay(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	now := time.Now()
	source := instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "standby-delay-src",
			Name:           "standby-delay-src",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateRunning,
	}

	mockMgr := newCaptureStandbyManager(svc.InstanceManager)
	mockMgr.result = &source
	svc.InstanceManager = mockMgr

	delay := "45s"
	resp, err := svc.StandbyInstance(
		mw.WithResolvedInstance(ctx(), source.Id, source),
		oapi.StandbyInstanceRequestObject{
			Id: source.Id,
			Body: &oapi.StandbyInstanceRequest{
				CompressionDelay: &delay,
			},
		},
	)
	require.NoError(t, err)
	_, ok := resp.(oapi.StandbyInstance200JSONResponse)
	require.True(t, ok, "expected 200 response")

	require.NotNil(t, mockMgr.lastReq)
	require.NotNil(t, mockMgr.lastReq.CompressionDelay)
	assert.Equal(t, 45*time.Second, *mockMgr.lastReq.CompressionDelay)
}

func TestStandbyInstance_InvalidCompressionDelay(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	now := time.Now()
	source := instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "standby-invalid-delay-src",
			Name:           "standby-invalid-delay-src",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
		},
		State: instances.StateRunning,
	}

	delay := "-5s"
	resp, err := svc.StandbyInstance(
		mw.WithResolvedInstance(ctx(), source.Id, source),
		oapi.StandbyInstanceRequestObject{
			Id: source.Id,
			Body: &oapi.StandbyInstanceRequest{
				CompressionDelay: &delay,
			},
		},
	)
	require.NoError(t, err)

	badReq, ok := resp.(oapi.StandbyInstance400JSONResponse)
	require.True(t, ok, "expected 400 response")
	assert.Equal(t, "invalid_compression_delay", badReq.Code)
	assert.Contains(t, badReq.Message, "compression_delay")
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

	mockMgr := newCaptureForkManager(svc.InstanceManager)
	mockMgr.result = forked
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
	imageName := createAndWaitForImage(t, svc, "docker.io/library/nginx:alpine", 60*time.Second)

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
			Image: imageName,
			Network: &struct {
				BandwidthDownload *string                                  `json:"bandwidth_download,omitempty"`
				BandwidthUpload   *string                                  `json:"bandwidth_upload,omitempty"`
				Egress            *oapi.CreateInstanceRequestNetworkEgress `json:"egress,omitempty"`
				Enabled           *bool                                    `json:"enabled,omitempty"`
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
