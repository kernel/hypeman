package portforward

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHandshakeRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteRequest(&buf, 9222))
	require.Equal(t, []byte{0x24, 0x06}, buf.Bytes(), "the port is two big-endian bytes")
	port, err := ReadRequest(&buf)
	require.NoError(t, err)
	require.EqualValues(t, 9222, port)

	for _, status := range []byte{StatusOK, StatusDialFailed} {
		require.NoError(t, WriteStatus(&buf, status))
		got, err := ReadStatus(&buf)
		require.NoError(t, err)
		require.Equal(t, status, got)
	}
}

func TestReadStatusRejectsUnknownBytes(t *testing.T) {
	_, err := ReadStatus(bytes.NewReader([]byte{7}))
	require.ErrorIs(t, err, ErrUnknownStatus)
}
