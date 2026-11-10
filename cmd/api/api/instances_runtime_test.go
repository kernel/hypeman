package api

import (
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/qemu"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestCreateInstanceMapsRequestedRuntimeVersion(t *testing.T) {
	svc := newTestService(t)
	manager := newCaptureCreateManager(svc.InstanceManager)
	svc.InstanceManager = manager
	runtime := oapi.CreateInstanceRequestHypervisor(hypervisor.TypeQEMU)
	version := qemu.VGPUSnapshotVersion
	response, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{Body: &oapi.CreateInstanceRequest{Name: "runtime-version-test", Image: "docker.io/library/alpine:latest", Hypervisor: &runtime, HypervisorVersion: &version}})
	require.NoError(t, err)
	require.IsType(t, oapi.CreateInstance201JSONResponse{}, response)
	require.Equal(t, version, manager.lastReq.HypervisorVersion)
}

func TestInstanceRuntimeVersionResponse(t *testing.T) {
	inst := instances.Instance{StoredMetadata: instances.StoredMetadata{HypervisorVersion: qemu.VGPUSnapshotVersion}}
	response := instanceToOAPI(inst)
	require.NotNil(t, response.HypervisorVersion)
	require.Equal(t, qemu.VGPUSnapshotVersion, *response.HypervisorVersion)
	inst.HypervisorVersion = ""
	require.Nil(t, instanceToOAPI(inst).HypervisorVersion)
}
