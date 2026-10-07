//go:build darwin

package vz

import (
	"encoding/base64"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMacOSStarterConfig(t *testing.T) {
	cfg := hypervisor.VMConfig{BootMode: hypervisor.BootModeMacOS, MacOS: &hypervisor.MacOSPlatform{HardwareModelData: []byte{1}, MachineIdentifierData: []byte{2}, AuxStoragePath: "/vm/aux.img"}, Disks: []hypervisor.DiskConfig{{Path: "/vm/root.raw"}}}
	require.NoError(t, NewStarter().ValidateConfig(cfg))
	shim := buildShimConfigFromVMConfig(cfg, "/vm/vz.sock")
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte{1}), shim.MacHardwareModelData)
	require.Equal(t, "/vm/aux.img", shim.MacAuxStoragePath)
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
