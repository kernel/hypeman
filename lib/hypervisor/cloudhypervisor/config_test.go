package cloudhypervisor

import (
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToVMConfigFileBackedMemory(t *testing.T) {
	t.Parallel()

	vmCfg := ToVMConfig(hypervisor.VMConfig{
		VCPUs:             2,
		MemoryBytes:       8 * 1024 * 1024 * 1024,
		MemoryBackingFile: "/var/lib/hypeman/memory.raw",
	})

	require.NotNil(t, vmCfg.Memory)
	assert.Zero(t, vmCfg.Memory.Size)
	require.NotNil(t, vmCfg.Memory.Zones)
	require.Len(t, *vmCfg.Memory.Zones, 1)
	zone := (*vmCfg.Memory.Zones)[0]
	assert.Equal(t, kernelPagingMemoryZoneID, zone.Id)
	assert.Equal(t, int64(8*1024*1024*1024), zone.Size)
	require.NotNil(t, zone.File)
	assert.Equal(t, "/var/lib/hypeman/memory.raw", *zone.File)
	require.NotNil(t, zone.Shared)
	assert.False(t, *zone.Shared)
	require.NotNil(t, zone.Prefault)
	assert.False(t, *zone.Prefault)
	require.NotNil(t, zone.Hugepages)
	assert.False(t, *zone.Hugepages)
}

func TestToVMConfigCPUHotplugEnvelope(t *testing.T) {
	t.Setenv(experimentalMaxVCPUsEnv, "8")

	vmCfg := ToVMConfig(hypervisor.VMConfig{
		VCPUs:       4,
		MemoryBytes: 1024 * 1024 * 1024,
		Topology: &hypervisor.CPUTopology{
			ThreadsPerCore: 2,
			CoresPerDie:    2,
			DiesPerPackage: 1,
			Packages:       1,
		},
	})

	require.NotNil(t, vmCfg.Cpus)
	assert.Equal(t, 4, vmCfg.Cpus.BootVcpus)
	assert.Equal(t, 8, vmCfg.Cpus.MaxVcpus)
	assert.Nil(t, vmCfg.Cpus.Topology)
}

func TestToVMConfigFileBackedHotplugMemory(t *testing.T) {
	t.Setenv(experimentalHotplugOverlayEnv, "true")

	vmCfg := ToVMConfig(hypervisor.VMConfig{
		VCPUs:             2,
		MemoryBytes:       4 * 1024 * 1024 * 1024,
		HotplugBytes:      4 * 1024 * 1024 * 1024,
		MemoryBackingFile: "/var/lib/hypeman/memory.raw",
	})

	require.NotNil(t, vmCfg.Memory)
	require.NotNil(t, vmCfg.Memory.Zones)
	require.Len(t, *vmCfg.Memory.Zones, 1)
	zone := (*vmCfg.Memory.Zones)[0]
	require.NotNil(t, zone.HotplugSize)
	assert.Equal(t, int64(4*1024*1024*1024), *zone.HotplugSize)
	assert.Nil(t, vmCfg.Memory.HotplugSize)
	require.NotNil(t, vmCfg.Memory.HotplugMethod)
	assert.Equal(t, "VirtioMem", *vmCfg.Memory.HotplugMethod)
}

func TestToVMConfig_GuestMemoryBalloon(t *testing.T) {
	cfg := hypervisor.VMConfig{
		VCPUs:       1,
		MemoryBytes: 512 * 1024 * 1024,
		GuestMemory: hypervisor.GuestMemoryConfig{
			EnableBalloon:     true,
			DeflateOnOOM:      true,
			FreePageReporting: true,
		},
	}

	vmCfg := ToVMConfig(cfg)
	require.NotNil(t, vmCfg.Balloon)
	assert.Equal(t, int64(0), vmCfg.Balloon.Size)
	require.NotNil(t, vmCfg.Balloon.DeflateOnOom)
	assert.True(t, *vmCfg.Balloon.DeflateOnOom)
	require.NotNil(t, vmCfg.Balloon.FreePageReporting)
	assert.True(t, *vmCfg.Balloon.FreePageReporting)
}
