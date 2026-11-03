//go:build darwin && arm64

package instances

import (
	"net"
	"testing"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/network"
	"github.com/stretchr/testify/require"
)

func testMacOSSource() *StoredMetadata {
	return &StoredMetadata{
		Id:             "source",
		NetworkEnabled: true,
		MAC:            "a6:fd:95:db:a2:62",
		MacOS: &images.MacOSImage{
			HardwareModel:     []byte("model"),
			MachineIdentifier: []byte("source-identifier"),
			MAC:               "a6:fd:95:db:a2:62",
			GuestAgent:        true,
		},
	}
}

func TestPrepareMacOSCloneRebindsIdentityWithoutAliasingSource(t *testing.T) {
	source := testMacOSSource()
	clone := cloneStoredMetadata(*source)

	require.NoError(t, prepareMacOSClone(&clone))

	require.True(t, clone.IdentityRekeyPending)
	require.NotEqual(t, []byte("source-identifier"), clone.MacOS.MachineIdentifier)
	require.NotEmpty(t, clone.MacOS.MachineIdentifier)
	require.Empty(t, clone.IP)

	// The source keeps its own identity; the clone's struct is independent.
	require.Equal(t, []byte("source-identifier"), source.MacOS.MachineIdentifier)
	require.Equal(t, "a6:fd:95:db:a2:62", source.MacOS.MAC)
	require.False(t, source.IdentityRekeyPending)
}

func TestPrepareMacOSCloneRefusesWithoutGuestAgent(t *testing.T) {
	for _, disable := range []func(*StoredMetadata){
		func(s *StoredMetadata) { s.MacOS.GuestAgent = false },
		func(s *StoredMetadata) { s.SkipGuestAgent = true },
	} {
		clone := testMacOSSource()
		disable(clone)
		require.ErrorIs(t, prepareMacOSClone(clone), ErrInvalidRequest)
		require.False(t, clone.IdentityRekeyPending)
	}
}

func TestNewUniqueMACAvoidsUsedAddresses(t *testing.T) {
	used := []string{"02:00:00:00:00:01", "02:00:00:00:00:02"}
	for i := 0; i < 64; i++ {
		mac, err := network.NewUniqueMAC(used)
		require.NoError(t, err)
		require.NotContains(t, used, mac)
		hw, err := net.ParseMAC(mac)
		require.NoError(t, err)
		require.NotZero(t, hw[0]&0x02, "locally administered bit set")
		require.Zero(t, hw[0]&0x01, "unicast")
	}
}
