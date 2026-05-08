package cloudhypervisor

import (
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestToVMConfig_SerialUsesSocket(t *testing.T) {
	cfg := hypervisor.VMConfig{
		VCPUs:         1,
		MemoryBytes:   512 * 1024 * 1024,
		SerialLogPath: "/var/lib/hypeman/guests/test/logs/app.log",
	}

	vmCfg := ToVMConfig(cfg)
	require.NotNil(t, vmCfg.Serial)
	assert.Equal(t, "Socket", string(vmCfg.Serial.Mode))
	require.NotNil(t, vmCfg.Serial.Socket)
	assert.Equal(t, "/var/lib/hypeman/guests/test/serial.sock", *vmCfg.Serial.Socket)
	assert.Nil(t, vmCfg.Serial.File)
}

func TestToVMConfig_SerialNullWhenNoLogPath(t *testing.T) {
	cfg := hypervisor.VMConfig{
		VCPUs:       1,
		MemoryBytes: 512 * 1024 * 1024,
	}

	vmCfg := ToVMConfig(cfg)
	require.NotNil(t, vmCfg.Serial)
	assert.Equal(t, "Null", string(vmCfg.Serial.Mode))
	assert.Nil(t, vmCfg.Serial.Socket)
	assert.Nil(t, vmCfg.Serial.File)
}
