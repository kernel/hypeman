package guest

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// unreachableDialer fails every dial the way a guest whose agent is not listening yet does.
type unreachableDialer struct{ key string }

func (d unreachableDialer) Key() string { return d.key }
func (d unreachableDialer) DialVsock(context.Context, int) (net.Conn, error) {
	return nil, &AgentVSockDialError{Err: errors.New("connect refused")}
}

func pooledKeyExists(key string) bool {
	connPool.RLock()
	defer connPool.RUnlock()
	_, ok := connPool.conns[key]
	return ok
}

// A probe owns its connection, so a failed probe leaves the pooled connection that
// other callers use in place. Ordinary exec still evicts after retryable failures.
func TestProbeExecLeavesPooledConnectionInPlace(t *testing.T) {
	const key = "probe-exec-test"
	t.Cleanup(func() { CloseConn(key) })
	shared := unreachableDialer{key: key}

	_, err := GetOrCreateConn(context.Background(), shared)
	require.NoError(t, err)
	require.True(t, pooledKeyExists(key))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = ProbeExec(ctx, shared, ExecOptions{Command: []string{"true"}, WaitForAgent: 50 * time.Millisecond})
	require.Error(t, err)
	require.True(t, pooledKeyExists(key), "a failed probe must not evict the pooled connection")

	_, err = ExecIntoInstance(ctx, shared, ExecOptions{Command: []string{"true"}, WaitForAgent: 50 * time.Millisecond})
	require.Error(t, err)
	require.False(t, pooledKeyExists(key), "ordinary exec still evicts after retryable failures")
}
