package qemu

import (
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStandardMachineTypeForArch(t *testing.T) {
	t.Parallel()
	assert.Equal(t, MachineTypeQ35, standardMachineTypeForArch("amd64"))
	assert.Equal(t, MachineTypeVirt, standardMachineTypeForArch("arm64"))
	assert.Panics(t, func() { standardMachineTypeForArch("riscv64") })
}

func TestResolveMachineTypeForPlatform(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		requested MachineType
		goos      string
		arch      string
		want      MachineType
		wantErr   bool
	}{
		{name: "linux amd64 accepts microvm", requested: MachineTypeMicroVM, goos: "linux", arch: "amd64", want: MachineTypeMicroVM},
		{name: "darwin amd64 rejects microvm", requested: MachineTypeMicroVM, goos: "darwin", arch: "amd64", wantErr: true},
		{name: "amd64 rejects virt", requested: MachineTypeVirt, goos: "linux", arch: "amd64", wantErr: true},
		{name: "arm64 rejects microvm", requested: MachineTypeMicroVM, goos: "linux", arch: "arm64", wantErr: true},
		{name: "arbitrary board rejected", requested: "q35,pcie=on", goos: "linux", arch: "amd64", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveMachineTypeForPlatform(tt.requested, tt.goos, tt.arch)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestStarterSelectsAndValidatesPrivateMachineType(t *testing.T) {
	t.Parallel()

	standard, err := machineTypeForHypervisor(hypervisor.TypeQEMU)
	require.NoError(t, err)
	assert.Equal(t, standardMachineType(), standard)

	if _, err := microVMMachineType(); err != nil {
		return
	}
	microvm, err := machineTypeForHypervisor(hypervisor.TypeQEMUMicroVM)
	require.NoError(t, err)
	assert.Equal(t, MachineTypeMicroVM, microvm)

	_, err = NewStarter().validateSnapshotMachineType(MachineTypeMicroVM)
	require.ErrorContains(t, err, "hypervisor qemu cannot use QEMU machine type")

	_, err = NewMicroVMStarter().validateSnapshotMachineType("")
	require.ErrorContains(t, err, "snapshot is missing")
}

func TestMicroVMCapabilitiesExcludePCIPassthrough(t *testing.T) {
	t.Parallel()
	assert.True(t, capabilities(hypervisor.TypeQEMU).SupportsGPUPassthrough)
	assert.False(t, capabilities(hypervisor.TypeQEMU).RequiresHostSnapshotVersion)
	assert.False(t, capabilities(hypervisor.TypeQEMUMicroVM).SupportsGPUPassthrough)
	assert.True(t, capabilities(hypervisor.TypeQEMUMicroVM).RequiresHostSnapshotVersion)
}

func TestValidateConfigMicroVM(t *testing.T) {
	t.Parallel()
	if _, err := microVMMachineType(); err != nil {
		t.Skipf("microvm is unavailable on this platform: %v", err)
	}
	base := hypervisor.VMConfig{Disks: make([]hypervisor.DiskConfig, 6), Networks: []hypervisor.NetworkConfig{{}}, VsockCID: 3}
	require.NoError(t, ValidateMicroVMConfig(base), "six disks, network, and vsock consume all eight slots")

	for _, tc := range []struct {
		name   string
		mutate func(*hypervisor.VMConfig)
	}{
		{name: "hotplug", mutate: func(cfg *hypervisor.VMConfig) { cfg.HotplugBytes = 1 }},
		{name: "pci", mutate: func(cfg *hypervisor.VMConfig) { cfg.PCIDevices = []string{"0000:00:01.0"} }},
		{name: "nine devices", mutate: func(cfg *hypervisor.VMConfig) { cfg.Disks = append(cfg.Disks, hypervisor.DiskConfig{}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			assert.Error(t, ValidateMicroVMConfig(cfg))
		})
	}
}
