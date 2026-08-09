package qemu

import (
	"fmt"
	"sync"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// clientPool manages singleton QMP connections per socket path.
// QEMU's QMP socket only allows one connection at a time, so we must
// reuse existing connections rather than creating new ones.
var clientPool = struct {
	sync.RWMutex
	clients map[string]*QEMU
}{
	clients: make(map[string]*QEMU),
}

// GetOrCreate returns a standard QEMU client for the socket path,
// or creates a new one if none exists.
func GetOrCreate(socketPath string) (*QEMU, error) {
	return GetOrCreateForType(socketPath, hypervisor.TypeQEMU)
}

// GetOrCreateForType returns a QEMU client with the requested backend identity.
func GetOrCreateForType(socketPath string, hypervisorType hypervisor.Type) (*QEMU, error) {
	// Try read lock first for existing connection
	clientPool.RLock()
	if client, ok := clientPool.clients[socketPath]; ok {
		clientPool.RUnlock()
		if client.hypervisorType != hypervisorType {
			return nil, poolTypeMismatchError(socketPath, client.hypervisorType, hypervisorType)
		}
		return client, nil
	}
	clientPool.RUnlock()

	// Need to create new connection - acquire write lock
	clientPool.Lock()
	defer clientPool.Unlock()

	// Double-check after acquiring write lock
	if client, ok := clientPool.clients[socketPath]; ok {
		if client.hypervisorType != hypervisorType {
			return nil, poolTypeMismatchError(socketPath, client.hypervisorType, hypervisorType)
		}
		return client, nil
	}

	// Create new client
	client, err := newClient(socketPath, hypervisorType)
	if err != nil {
		return nil, err
	}

	clientPool.clients[socketPath] = client
	return client, nil
}

func poolTypeMismatchError(socketPath string, cached, requested hypervisor.Type) error {
	return fmt.Errorf("QEMU client for %s is pooled as hypervisor %s, not %s", socketPath, cached, requested)
}

// Remove closes and removes a client from the pool.
// Called automatically on errors to allow fresh reconnection.
// Close is done asynchronously to avoid blocking if the connection is in a bad state.
func Remove(socketPath string) {
	clientPool.Lock()
	defer clientPool.Unlock()

	if client, ok := clientPool.clients[socketPath]; ok {
		delete(clientPool.clients, socketPath)
		// Close asynchronously to avoid blocking on stuck connections
		go client.client.Close()
	}
}
