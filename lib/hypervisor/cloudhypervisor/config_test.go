package cloudhypervisor

import (
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToVMConfig_VGPU(t *testing.T) {
	t.Parallel()

	path := "/sys/bus/mdev/devices/aa618089-8b16-4d01-a136-25a0f3c73123"
	vmCfg := ToVMConfig(hypervisor.VMConfig{VGPUDevicePath: path})

	require.NotNil(t, vmCfg.Devices)
	require.Len(t, *vmCfg.Devices, 1)
	assert.Equal(t, path, (*vmCfg.Devices)[0].Path)
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
