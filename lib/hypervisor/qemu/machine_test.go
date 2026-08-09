package qemu

import (
	"runtime"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveMachineTypeForPlatform(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		requested hypervisor.MachineType
		goos      string
		arch      string
		want      hypervisor.MachineType
		wantErr   bool
	}{
		{name: "linux amd64 defaults q35", goos: "linux", arch: "amd64", want: MachineTypeQ35},
		{name: "linux amd64 accepts microvm", requested: MachineTypeMicroVM, goos: "linux", arch: "amd64", want: MachineTypeMicroVM},
		{name: "darwin amd64 rejects microvm", requested: MachineTypeMicroVM, goos: "darwin", arch: "amd64", wantErr: true},
		{name: "amd64 rejects virt", requested: MachineTypeVirt, goos: "linux", arch: "amd64", wantErr: true},
		{name: "arm64 defaults virt", goos: "linux", arch: "arm64", want: MachineTypeVirt},
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

func TestStarterAppliesBackendMachineType(t *testing.T) {
	t.Parallel()

	standard, err := NewStarter().applyMachineType(hypervisor.VMConfig{}, false)
	require.NoError(t, err)
	expectedStandard, err := ResolveMachineType("")
	require.NoError(t, err)
	assert.Equal(t, expectedStandard, standard.MachineType)

	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return
	}
	microvm, err := NewMicroVMStarter().applyMachineType(hypervisor.VMConfig{}, false)
	require.NoError(t, err)
	assert.Equal(t, MachineTypeMicroVM, microvm.MachineType)

	_, err = NewStarter().applyMachineType(hypervisor.VMConfig{MachineType: MachineTypeMicroVM}, false)
	require.ErrorContains(t, err, "requires hypervisor qemu-microvm")

	_, err = NewMicroVMStarter().applyMachineType(hypervisor.VMConfig{}, true)
	require.ErrorContains(t, err, "snapshot is missing")
}

func TestMicroVMCapabilitiesExcludePCIPassthrough(t *testing.T) {
	t.Parallel()
	assert.True(t, capabilities(hypervisor.TypeQEMU).SupportsGPUPassthrough)
	assert.False(t, capabilities(hypervisor.TypeQEMUMicroVM).SupportsGPUPassthrough)
}

func TestValidateConfigMicroVM(t *testing.T) {
	t.Parallel()
	base := hypervisor.VMConfig{MachineType: MachineTypeMicroVM, Disks: make([]hypervisor.DiskConfig, 6), Networks: []hypervisor.NetworkConfig{{}}, VsockCID: 3}
	require.NoError(t, ValidateConfig(base), "six disks, network, and vsock consume all eight slots")

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
			assert.Error(t, ValidateConfig(cfg))
		})
	}
}
