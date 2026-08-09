package instances

import (
	"errors"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/qemu"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveCreateMachineType(t *testing.T) {
	t.Parallel()
	m := &manager{}

	t.Run("qemu uses architecture default", func(t *testing.T) {
		machine, err := m.resolveCreateMachineType(CreateInstanceRequest{}, hypervisor.TypeQEMU)
		require.NoError(t, err)
		want, err := qemu.ResolveMachineType("")
		require.NoError(t, err)
		assert.Equal(t, want, machine)
	})

	t.Run("non qemu has no machine type", func(t *testing.T) {
		machine, err := m.resolveCreateMachineType(CreateInstanceRequest{}, hypervisor.TypeFirecracker)
		require.NoError(t, err)
		assert.Empty(t, machine)
	})

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
			_, err := m.resolveCreateMachineType(tc.req, hypervisor.TypeQEMUMicroVM)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidRequest))
		})
	}

	machine, err := m.resolveCreateMachineType(CreateInstanceRequest{
		Volumes: []VolumeAttachment{{}, {}, {}, {}},
	}, hypervisor.TypeQEMUMicroVM)
	require.NoError(t, err)
	assert.Equal(t, qemu.MachineTypeMicroVM, machine)
}

func TestNormalizeSnapshotMachineType(t *testing.T) {
	t.Parallel()

	machine, err := normalizeSnapshotMachineType(qemu.MachineTypeMicroVM, hypervisor.TypeQEMUMicroVM, hypervisor.TypeQEMUMicroVM)
	require.NoError(t, err)
	assert.Equal(t, qemu.MachineTypeMicroVM, machine)

	machine, err = normalizeSnapshotMachineType(qemu.MachineTypeMicroVM, hypervisor.TypeQEMUMicroVM, hypervisor.TypeQEMU)
	require.NoError(t, err)
	standard, err := qemu.ResolveMachineType("")
	require.NoError(t, err)
	assert.Equal(t, standard, machine)

	machine, err = normalizeSnapshotMachineType(standard, hypervisor.TypeQEMU, hypervisor.TypeQEMUMicroVM)
	require.NoError(t, err)
	assert.Equal(t, qemu.MachineTypeMicroVM, machine)

	machine, err = normalizeSnapshotMachineType(qemu.MachineTypeMicroVM, hypervisor.TypeQEMUMicroVM, hypervisor.TypeFirecracker)
	require.NoError(t, err)
	assert.Empty(t, machine)
}
