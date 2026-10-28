package api

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHostMachineImageCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, os, arch, defaultPlatform string
		emulation, macOSOnly            bool
		platforms                       []string
	}{
		{"Linux unchanged", "linux", "amd64", "linux/amd64", false, false, []string{"linux/amd64"}},
		{"Linux arm unchanged", "linux", "arm64", "linux/arm64", false, false, []string{"linux/arm64"}},
		{"Mac mixed", "darwin", "arm64", "linux/arm64", false, false, []string{"linux/arm64", "darwin/arm64"}},
		{"Mac mixed Rosetta", "darwin", "arm64", "linux/arm64", true, false, []string{"linux/arm64", "linux/amd64", "darwin/arm64"}},
		{"Mac only", "darwin", "arm64", "darwin/arm64", true, true, []string{"darwin/arm64"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := hostImageCapabilities(tc.os, tc.arch, tc.emulation, tc.macOSOnly)
			require.Equal(t, tc.defaultPlatform, caps.DefaultPlatform)
			require.Equal(t, tc.platforms, caps.Platforms)
		})
	}
}

func TestMachineImageFeatureActivation(t *testing.T) {
	require.Empty(t, machineImageFeatures("linux", "arm64", true))
	require.Empty(t, machineImageFeatures("darwin", "amd64", true))
	require.Equal(t, []string{"macos-machine-images"}, machineImageFeatures("darwin", "arm64", false))
	require.Equal(t, []string{"macos-machine-images", "macos-machine-builds-v1"}, machineImageFeatures("darwin", "arm64", true))
}
