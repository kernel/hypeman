package mailbox

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sliceWriterAt []byte

func (w sliceWriterAt) WriteAt(p []byte, off int64) (int, error) {
	copy(w[off:], p)
	return len(p), nil
}

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

func TestForkMailboxMarker(t *testing.T) {
	t.Parallel()

	marker, err := ForkMailboxMarker("kernel.identity.v1", "template-token")
	require.NoError(t, err)
	assert.Equal(t, ForkMailboxMagic+"kernel.identity.v1\x00template-token", string(marker))

	_, err = ForkMailboxMarker("", "template-token")
	require.Error(t, err)

	_, err = ForkMailboxMarker("kernel.identity.v1", "")
	require.Error(t, err)
}

func TestWriteForkMailboxPayloadFrame(t *testing.T) {
	t.Parallel()

	buf := make([]byte, ForkMailboxSize)
	payload := []byte(`{"instance_name":"forked"}`)
	require.NoError(t, WritePayloadAt(sliceWriterAt(buf), ForkLayout, 512, payload))

	assert.Equal(t, uint32(1), binary.LittleEndian.Uint32(buf[512+ForkMailboxSeqOffset:]))
	payloadLen := binary.LittleEndian.Uint32(buf[512+ForkMailboxLengthOffset:])
	assert.Equal(t, uint32(len(payload)), payloadLen)
	assert.Equal(t, string(payload), string(buf[512+ForkMailboxPayloadOffset:512+ForkMailboxPayloadOffset+int(payloadLen)]))
}
