package qemu

import (
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMacOSBootRejectedByQEMU(t *testing.T) {
	err := validateProfileCapabilities(hypervisor.TypeQEMU, hypervisor.Capabilities{SupportsUEFIBoot: true}, hypervisor.VMConfig{BootMode: hypervisor.BootModeMacOS})
	require.ErrorContains(t, err, "does not support macOS boot")
}
