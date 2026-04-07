//go:build linux

package autostandby

import (
	"encoding/binary"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func TestConnectionEventFromNetlinkMessageParsesIPv4TCPEvent(t *testing.T) {
	t.Parallel()

	msg := syscall.NetlinkMessage{
		Header: syscall.NlMsghdr{Type: 0x100 | uint16(0)},
		Data: []byte{
			2, 0, 0, 0,
			52, 0, 1, 128,
			20, 0, 1, 128,
			8, 0, 1, 0, 192, 168, 0, 10,
			8, 0, 2, 0, 192, 168, 77, 73,
			28, 0, 2, 128,
			5, 0, 1, 0, 6, 0, 0, 0,
			6, 0, 2, 0, 166, 129, 0, 0,
			6, 0, 3, 0, 13, 5, 0, 0,
			48, 0, 4, 128,
			44, 0, 1, 128,
			5, 0, 1, 0, 8, 0, 0, 0,
			5, 0, 2, 0, 0, 0, 0, 0,
			5, 0, 3, 0, 0, 0, 0, 0,
			6, 0, 4, 0, 39, 0, 0, 0,
			6, 0, 5, 0, 32, 0, 0, 0,
		},
	}

	event, ok, err := connectionEventFromNetlinkMessage(msg)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, ConnectionEventNew, event.Type)
	assert.Equal(t, mustAddr("192.168.0.10"), event.Connection.OriginalSourceIP)
	assert.Equal(t, uint16(42625), event.Connection.OriginalSourcePort)
	assert.Equal(t, mustAddr("192.168.77.73"), event.Connection.OriginalDestinationIP)
	assert.Equal(t, uint16(3333), event.Connection.OriginalDestinationPort)
	assert.Equal(t, TCPStateClose, event.Connection.TCPState)
}

func TestConnectionEventFromNetlinkMessageParsesNativeEndianNLMSGError(t *testing.T) {
	t.Parallel()

	data := make([]byte, 4)
	errno := int32(-int(unix.EPERM))
	nl.NativeEndian().PutUint32(data, uint32(errno))

	_, ok, err := connectionEventFromNetlinkMessage(syscall.NetlinkMessage{
		Header: syscall.NlMsghdr{Type: unix.NLMSG_ERROR},
		Data:   data,
	})
	require.ErrorIs(t, err, unix.EPERM)
	require.False(t, ok)

	// Sanity-check the fixture is using native byte order rather than an accidental little-endian match.
	if nl.NativeEndian() != binary.LittleEndian {
		require.NotEqual(t, binary.LittleEndian.Uint32(data), nl.NativeEndian().Uint32(data))
	}
}
