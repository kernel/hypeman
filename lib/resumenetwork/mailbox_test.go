package resumenetwork

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarker(t *testing.T) {
	t.Parallel()

	marker, err := Marker("token")
	require.NoError(t, err)
	assert.Equal(t, MailboxMagic+"token", string(marker))

	_, err = Marker("")
	require.Error(t, err)

	_, err = Marker(strings.Repeat("x", MailboxTokenMaxLen+1))
	require.Error(t, err)
}

func TestPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	want := &Payload{
		InterfaceName: "eth0",
		MAC:           "02:00:00:85:17:c8",
		IPv4:          "10.102.146.62",
		Prefix:        16,
		Gateway:       "10.102.0.1",
		AckPort:       43210,
	}
	payloadBytes, err := MarshalPayload(want)
	require.NoError(t, err)

	buf := make([]byte, MailboxSize)
	copy(buf[MailboxPayloadOffset:], payloadBytes)
	binary.LittleEndian.PutUint32(buf[MailboxLengthOffset:], uint32(len(payloadBytes)))

	got, err := DecodePayloadFrame(buf, binary.LittleEndian.Uint32(buf[MailboxLengthOffset:]))
	require.NoError(t, err)
	assert.Equal(t, *want, got)
}
