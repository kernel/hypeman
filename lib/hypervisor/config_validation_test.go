package hypervisor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateBootConfigPreservesDirectRawDefaults(t *testing.T) {
	cfg := VMConfig{
		KernelPath: "/kernel",
		Disks:      []DiskConfig{{Path: "/rootfs"}},
	}
	require.NoError(t, ValidateBootConfig(cfg))
	assert.Equal(t, BootModeDirect, cfg.EffectiveBootMode())
	assert.Equal(t, DiskFormatRaw, cfg.Disks[0].EffectiveFormat())
}

func TestValidateBootConfigUEFI(t *testing.T) {
	valid := VMConfig{
		BootMode: BootModeUEFI,
		Firmware: &FirmwareConfig{CodePath: "/ovmf/code", VarsPath: "/instance/vars"},
		TPM:      &TPMConfig{SocketPath: "/instance/swtpm.sock", StateDir: "/instance/tpm"},
		Disks:    []DiskConfig{{Path: "/instance/disk", Format: DiskFormatQCOW2}},
	}
	require.NoError(t, ValidateBootConfig(valid))

	tests := []struct {
		name string
		cfg  VMConfig
	}{
		{name: "missing firmware", cfg: VMConfig{BootMode: BootModeUEFI}},
		{name: "direct kernel", cfg: VMConfig{BootMode: BootModeUEFI, Firmware: valid.Firmware, KernelPath: "/kernel"}},
		{name: "incomplete TPM", cfg: VMConfig{BootMode: BootModeUEFI, Firmware: valid.Firmware, TPM: &TPMConfig{StateDir: "/state"}}},
		{name: "unknown disk", cfg: VMConfig{Disks: []DiskConfig{{Path: "/disk", Format: "vhdx"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Error(t, ValidateBootConfig(tt.cfg))
		})
	}
}

func TestValidateDirectRawConfigRejectsFirmwareAndQCOW2(t *testing.T) {
	uefi := VMConfig{BootMode: BootModeUEFI, Firmware: &FirmwareConfig{CodePath: "/code", VarsPath: "/vars"}}
	assert.ErrorContains(t, ValidateDirectRawConfig("backend", uefi), "does not support uefi boot")

	qcow := VMConfig{Disks: []DiskConfig{{Path: "/disk", Format: DiskFormatQCOW2}}}
	assert.ErrorContains(t, ValidateDirectRawConfig("backend", qcow), "does not support disk 0 format")
}
