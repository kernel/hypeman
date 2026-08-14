package network

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGuestToGuestEnabled(t *testing.T) {
	t.Parallel()
	require.False(t, GuestToGuestEnabled(&Network{Isolated: true}),
		"isolated default network blocks direct guest-to-guest traffic")
	if NetworkModel() == "bridge" {
		require.True(t, GuestToGuestEnabled(&Network{Isolated: false}))
	} else {
		require.False(t, GuestToGuestEnabled(&Network{Isolated: false}),
			"NAT networking never provides direct guest-to-guest reachability")
	}
}
