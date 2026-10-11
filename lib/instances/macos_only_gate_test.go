package instances

import (
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/stretchr/testify/require"
)

func TestMacOSOnlyServerRefusesLinuxRestoreAndFork(t *testing.T) {
	m := &manager{macOSOnly: true}
	require.ErrorIs(t, m.requireMacOSGuest(&StoredMetadata{}), ErrInvalidRequest)
	require.NoError(t, m.requireMacOSGuest(&StoredMetadata{MacOS: &images.MacOSImage{}}))

	mixed := &manager{}
	require.NoError(t, mixed.requireMacOSGuest(&StoredMetadata{}), "a mixed host keeps Linux guests")
}

func TestMacOSCreateAcceptsExplicitResources(t *testing.T) {
	img := &images.Image{Platform: "darwin/arm64", SizeBytes: testInt64Ptr(1 << 30), MacOS: &images.MacOSImage{Memory: 8 << 30, CPUs: 4}}
	req := CreateInstanceRequest{Size: 4 << 30}
	require.NoError(t, prepareMacOSCreate(&req, img, testMacOSCaps()))
	require.Equal(t, int64(4<<30), req.Size)

	req = CreateInstanceRequest{Vcpus: 8}
	require.NoError(t, prepareMacOSCreate(&req, img, testMacOSCaps()))
	require.Equal(t, 8, req.Vcpus)
	require.Equal(t, uint(4), img.MacOS.CPUs)
	require.Equal(t, uint64(8<<30), img.MacOS.Memory)

	req = CreateInstanceRequest{Size: 8 << 30, Vcpus: 4}
	require.NoError(t, prepareMacOSCreate(&req, img, testMacOSCaps()), "matching values are accepted")
}

func testMacOSCaps() hypervisor.Capabilities {
	return hypervisor.Capabilities{SupportsMacOSBoot: true}
}

func testInt64Ptr(v int64) *int64 { return &v }
