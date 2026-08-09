package instances

import (
	"errors"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/qemu"
	"github.com/stretchr/testify/require"
)

func TestValidateQEMUMicroVMCreate(t *testing.T) {
	t.Parallel()
	requireQEMUMicroVMValidationPlatform(t)
	m := &manager{}

	require.NoError(t, m.validateQEMUMicroVMCreate(CreateInstanceRequest{}, hypervisor.TypeQEMU))

	for _, tc := range []struct {
		name string
		req  CreateInstanceRequest
	}{
		{name: "hotplug", req: CreateInstanceRequest{HotplugSize: 1}},
		{name: "pci", req: CreateInstanceRequest{Devices: []string{"gpu"}}},
		{name: "vgpu", req: CreateInstanceRequest{GPU: &GPUConfig{Profile: "L40S-1Q"}}},
		{name: "nine mmio devices", req: CreateInstanceRequest{Volumes: []VolumeAttachment{{}, {}, {}, {}, {}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := m.validateQEMUMicroVMCreate(tc.req, hypervisor.TypeQEMUMicroVM)
			require.Error(t, err)
			require.True(t, errors.Is(err, ErrInvalidRequest))
		})
	}

	require.NoError(t, m.validateQEMUMicroVMCreate(CreateInstanceRequest{
		Volumes: []VolumeAttachment{{}, {}, {}, {}},
	}, hypervisor.TypeQEMUMicroVM))
}

func TestValidateQEMUMicroVMMetadata(t *testing.T) {
	t.Parallel()
	requireQEMUMicroVMValidationPlatform(t)
	m := &manager{}

	require.NoError(t, m.validateQEMUMicroVMMetadata(StoredMetadata{}, hypervisor.TypeQEMU))

	for _, meta := range []StoredMetadata{
		{HotplugSize: 1},
		{Devices: []string{"gpu"}},
		{GPUProfile: "L40S-1Q"},
		{Volumes: []VolumeAttachment{{}, {}, {}, {}, {}}},
	} {
		err := m.validateQEMUMicroVMMetadata(meta, hypervisor.TypeQEMUMicroVM)
		require.Error(t, err)
		require.True(t, errors.Is(err, ErrInvalidRequest))
	}
}

func requireQEMUMicroVMValidationPlatform(t *testing.T) {
	t.Helper()
	if _, err := qemu.ResolveMachineType(qemu.MachineTypeMicroVM); err != nil {
		t.Skipf("microvm is unavailable on this platform: %v", err)
	}
}
