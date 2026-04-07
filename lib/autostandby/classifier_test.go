package autostandby

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActiveInboundCountCountsOnlyQualifyingInboundTCP(t *testing.T) {
	t.Parallel()

	inst := Instance{
		ID:    "inst-1",
		IP:    "192.168.100.10",
		State: StateRunning,
		AutoStandby: &Policy{
			Enabled:                true,
			IdleTimeout:            "5m",
			IgnoreSourceCIDRs:      []string{"10.0.0.0/8"},
			IgnoreDestinationPorts: []uint16{22},
		},
	}

	count, idleTimeout, err := ActiveInboundCount(inst, []Connection{
		{
			OriginalSourceIP:        netip.MustParseAddr("1.2.3.4"),
			OriginalDestinationIP:   netip.MustParseAddr("192.168.100.10"),
			OriginalDestinationPort: 8080,
			TCPState:                TCPStateEstablished,
		},
		{
			OriginalSourceIP:        netip.MustParseAddr("192.168.100.10"),
			OriginalDestinationIP:   netip.MustParseAddr("8.8.8.8"),
			OriginalDestinationPort: 443,
			TCPState:                TCPStateEstablished,
		},
		{
			OriginalSourceIP:        netip.MustParseAddr("10.1.2.3"),
			OriginalDestinationIP:   netip.MustParseAddr("192.168.100.10"),
			OriginalDestinationPort: 8080,
			TCPState:                TCPStateEstablished,
		},
		{
			OriginalSourceIP:        netip.MustParseAddr("5.6.7.8"),
			OriginalDestinationIP:   netip.MustParseAddr("192.168.100.10"),
			OriginalDestinationPort: 22,
			TCPState:                TCPStateEstablished,
		},
		{
			OriginalSourceIP:        netip.MustParseAddr("9.9.9.9"),
			OriginalDestinationIP:   netip.MustParseAddr("192.168.100.10"),
			OriginalDestinationPort: 8080,
			TCPState:                TCPStateTimeWait,
		},
	})
	require.NoError(t, err)

	assert.Equal(t, 1, count)
	assert.Equal(t, 5*time.Minute, idleTimeout)
}

func TestActiveInboundCountRejectsInvalidInstanceIP(t *testing.T) {
	t.Parallel()

	_, _, err := ActiveInboundCount(Instance{
		IP: "not-an-ip",
		AutoStandby: &Policy{
			Enabled:     true,
			IdleTimeout: "5m",
		},
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse instance IP")
}
