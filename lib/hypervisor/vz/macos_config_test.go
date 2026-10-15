//go:build darwin

package vz

import (
	"encoding/base64"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMacOSStarterConfig(t *testing.T) {
	cfg := hypervisor.VMConfig{BootMode: hypervisor.BootModeMacOS, VCPUs: 2, MemoryBytes: 4 << 30, MacOS: &hypervisor.MacOSPlatform{HardwareModelData: []byte{1}, MachineIdentifierData: []byte{2}, AuxStoragePath: "/vm/aux.img"}, Disks: []hypervisor.DiskConfig{{Path: "/vm/root.raw"}}}
	require.NoError(t, NewStarter().ValidateConfig(cfg))
	shim := buildShimConfigFromVMConfig(cfg, "/vm/vz.sock")
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte{1}), shim.MacHardwareModelData)
	require.Equal(t, "/vm/aux.img", shim.MacAuxStoragePath)
	for _, resources := range []struct {
		cpus   int
		memory int64
	}{{1, 4 << 30}, {2, 1 << 30}, {2, (4 << 30) + 1}} {
		bad := cfg
		bad.VCPUs, bad.MemoryBytes = resources.cpus, resources.memory
		require.Error(t, NewStarter().ValidateConfig(bad))
	}
	bad := cfg
	bad.KernelPath = "/linux"
	require.Error(t, NewStarter().ValidateConfig(bad))
	bad = cfg
	bad.GuestMemory.EnableBalloon = true
	require.Error(t, NewStarter().ValidateConfig(bad))
	bad = cfg
	bad.Disks = []hypervisor.DiskConfig{{Readonly: true}}
	require.Error(t, NewStarter().ValidateConfig(bad))
	bad = cfg
	bad.MacOS = nil
	require.Error(t, NewStarter().ValidateConfig(bad))
}
