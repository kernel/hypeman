package instances

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBoundedBufferCapsAndAcceptsEverything(t *testing.T) {
	b := boundedBuffer{limit: 8}
	n, err := b.Write([]byte("0123456789"))
	require.NoError(t, err)
	require.Equal(t, 10, n, "writes report full acceptance so the guest stream is never stalled")
	for i := 0; i < 1000; i++ {
		_, _ = b.Write([]byte(strings.Repeat("x", 1024)))
	}
	require.Equal(t, "01234567", b.String())
	require.LessOrEqual(t, len(b.buf), 8)
}
