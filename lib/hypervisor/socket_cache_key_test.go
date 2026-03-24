package hypervisor

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSocketCacheKeyChangesWhenSocketIsRecreated(t *testing.T) {
	socketPath := fmt.Sprintf("/tmp/hypeman-socket-key-%d.sock", time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	firstKey := SocketCacheKey(socketPath)
	require.NotEmpty(t, firstKey)
	require.NoError(t, listener.Close())

	listener, err = net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()
	secondKey := SocketCacheKey(socketPath)
	require.NotEmpty(t, secondKey)

	require.NotEqual(t, firstKey, secondKey)
}
