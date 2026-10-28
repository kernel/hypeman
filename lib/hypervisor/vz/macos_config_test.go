//go:build darwin

package vz

import (
	"encoding/base64"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMacOSStarterConfig(t *testing.T) {
	cfg := hypervisor.VMConfig{BootMode: hypervisor.BootModeMacOS, VCPUs: 2, MemoryBytes: 4 << 30, MacOS: &hypervisor.MacOSPlatform{HardwareModelData: []byte{1}, MachineIdentifierData: []byte{2}, AuxStoragePath: "/vm/aux.img"}, Disks: []hypervisor.DiskConfig{{Path: "/vm/root.raw"}}}
	require.NoError(t, NewStarter().ValidateConfig(cfg))
	shim := buildShimConfigFromVMConfig(cfg, "/vm/vz.sock")
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte{1}), shim.MacHardwareModelData)
	require.Equal(t, "/vm/aux.img", shim.MacAuxStoragePath)
	require.Equal(t, "/vm/vz-guest-stopped", shim.StopReceiptPath)
	require.Empty(t, buildShimConfigFromVMConfig(hypervisor.VMConfig{}, "/vm/vz.sock").StopReceiptPath, "Linux guests write no receipt")
	networked := cfg
	networked.Networks = []hypervisor.NetworkConfig{{MAC: "02:00:00:00:00:01", IP: "192.168.128.10", Netmask: "255.255.255.248"}}
	require.Equal(t, []shimconfig.NetworkConfig{{MAC: "02:00:00:00:00:01", IPv4Subnet: "192.168.128.8/29", IPv4Address: "192.168.128.10"}}, buildShimConfigFromVMConfig(networked, "/vm/vz.sock").Networks)
	linux := hypervisor.VMConfig{Networks: networked.Networks}
	require.Empty(t, buildShimConfigFromVMConfig(linux, "/vm/vz.sock").Networks[0].IPv4Address, "Linux guests keep the shared NAT")
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
