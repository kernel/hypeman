//go:build darwin && arm64

package main

import (
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMacBootRequiresCompleteIdentity(t *testing.T) {
	for _, c := range []shimconfig.ShimConfig{
		{MacHardwareModelData: "AA=="},
		{MacMachineIdentifierData: "AA=="},
		{MacAuxStoragePath: "/missing"},
	} {
		_, _, err := createVM(&c)
		require.ErrorContains(t, err, "macOS boot requires")
	}
}
func TestMacPlatformRejectsMalformedModel(t *testing.T) {
	err := configureMacPlatform(nil, &shimconfig.ShimConfig{MacHardwareModelData: "not base64!"})
	require.ErrorContains(t, err, "decode Mac hardware model")
}
