package qemu

import (
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
)

func TestGetOrCreateForTypeRejectsCachedBackendMismatch(t *testing.T) {
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
	require.ErrorContains(t, err, "pooled as hypervisor qemu, not qemu-microvm")
}
