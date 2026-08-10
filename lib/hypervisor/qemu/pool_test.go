package qemu

import (
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
)

func TestRemoveClientDoesNotEvictReplacement(t *testing.T) {
	socketPath := t.TempDir() + "/qemu.sock"
	stale := &QEMU{socketPath: socketPath, profile: StandardProfile{}}
	replacement := &QEMU{socketPath: socketPath, profile: MicroVMProfile{}}
	clientPool.Lock()
	clientPool.clients[socketPath] = replacement
	clientPool.Unlock()
	t.Cleanup(func() {
		clientPool.Lock()
		delete(clientPool.clients, socketPath)
		clientPool.Unlock()
	})

	removeClient(stale)

	clientPool.RLock()
	got := clientPool.clients[socketPath]
	clientPool.RUnlock()
	require.Same(t, replacement, got)
}

func TestGetOrCreateForTypeRejectsCachedBackendMismatch(t *testing.T) {
	socketPath := t.TempDir() + "/qemu.sock"
	clientPool.Lock()
	clientPool.clients[socketPath] = &QEMU{socketPath: socketPath, profile: StandardProfile{}}
	clientPool.Unlock()
	t.Cleanup(func() {
		clientPool.Lock()
		delete(clientPool.clients, socketPath)
		clientPool.Unlock()
	})

	_, err := GetOrCreateForType(socketPath, hypervisor.TypeQEMUMicroVM)
	require.ErrorContains(t, err, "pooled as hypervisor qemu, not qemu-microvm")

	clientPool.RLock()
	cached := clientPool.clients[socketPath]
	clientPool.RUnlock()
	require.Equal(t, hypervisor.TypeQEMU, cached.profile.hypervisorType(), "a stale caller must not evict the live client")
}
