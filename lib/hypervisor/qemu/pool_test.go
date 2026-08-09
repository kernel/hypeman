package qemu

import (
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
)

func TestGetOrCreateForTypeReplacesCachedBackendMismatch(t *testing.T) {
	socketPath := t.TempDir() + "/qemu.sock"
	clientPool.Lock()
	clientPool.clients[socketPath] = &QEMU{socketPath: socketPath, hypervisorType: hypervisor.TypeQEMU}
	clientPool.Unlock()
	t.Cleanup(func() {
		clientPool.Lock()
		delete(clientPool.clients, socketPath)
		clientPool.Unlock()
	})

	_, err := GetOrCreateForType(socketPath, hypervisor.TypeQEMUMicroVM)
	require.ErrorContains(t, err, "create qemu client")

	clientPool.RLock()
	_, stillCached := clientPool.clients[socketPath]
	clientPool.RUnlock()
	require.False(t, stillCached, "stale backend client must be removed before reconnect")
}
