package network

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNetworkModelIsTyped(t *testing.T) {
	t.Parallel()
	model := NetworkModel()
	require.Contains(t, []Model{ModelBridge, ModelNAT}, model)
}

func TestGuestToGuestEnabled(t *testing.T) {
	t.Parallel()
	require.False(t, GuestToGuestEnabled(nil))
	require.False(t, GuestToGuestEnabled(&Network{Isolated: true}),
		"isolated default network blocks direct guest-to-guest traffic")
	if NetworkModel() == ModelBridge {
		require.True(t, GuestToGuestEnabled(&Network{Isolated: false}),
			"a non-isolated bridge network permits direct guest-to-guest traffic")
	} else {
		require.False(t, GuestToGuestEnabled(&Network{Isolated: false}),
			"NAT networking never provides direct guest-to-guest reachability")
	}
}
