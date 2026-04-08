//go:build linux

package autostandby

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestConnectionFromFlowNormalizesTCPConntrackEntry(t *testing.T) {
	t.Parallel()

	conn, ok := connectionFromFlow(&netlink.ConntrackFlow{
		Forward: netlink.IPTuple{
			Protocol: unix.IPPROTO_TCP,
			SrcIP:    net.ParseIP("1.2.3.4").To4(),
			SrcPort:  12345,
			DstIP:    net.ParseIP("192.168.100.10").To4(),
			DstPort:  8080,
		},
		ProtoInfo: &netlink.ProtoInfoTCP{State: uint8(TCPStateEstablished)},
	})
	require.True(t, ok)

	assert.Equal(t, mustAddr("1.2.3.4"), conn.OriginalSourceIP)
	assert.Equal(t, uint16(12345), conn.OriginalSourcePort)
	assert.Equal(t, mustAddr("192.168.100.10"), conn.OriginalDestinationIP)
	assert.Equal(t, uint16(8080), conn.OriginalDestinationPort)
	assert.Equal(t, TCPStateEstablished, conn.TCPState)
}
