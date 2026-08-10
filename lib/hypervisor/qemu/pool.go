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
	requestedProfile, err := profileForType(hypervisorType)
	if err != nil {
		return nil, err
	}

	clientPool.RLock()
	if client, ok := clientPool.clients[socketPath]; ok {
		clientPool.RUnlock()
		if client.profile.hypervisorType() != hypervisorType {
			return nil, poolTypeMismatchError(socketPath, client.profile.hypervisorType(), hypervisorType)
		}
		return client, nil
	}
	clientPool.RUnlock()

	clientPool.Lock()
	if client, ok := clientPool.clients[socketPath]; ok {
		clientPool.Unlock()
		if client.profile.hypervisorType() != hypervisorType {
			return nil, poolTypeMismatchError(socketPath, client.profile.hypervisorType(), hypervisorType)
		}
		return client, nil
	}

	client, err := newClient(socketPath, requestedProfile)
	if err != nil {
		clientPool.Unlock()
		return nil, err
	}
	clientPool.clients[socketPath] = client
	clientPool.Unlock()
	return client, nil
}

func poolTypeMismatchError(socketPath string, cached, requested hypervisor.Type) error {
	return fmt.Errorf("QEMU client for %s is pooled as hypervisor %s, not %s", socketPath, cached, requested)
}

// resetClient drops a pooled connection before a new QEMU process reuses the
// same socket path. Disconnect happens outside the pool lock and stale users
// can only remove their own client generation.
func resetClient(socketPath string) {
	client := takeClient(socketPath, nil)
	closeClientAsync(client)
}

// removeClient removes client only if it is still the current generation for
// its socket path. This prevents a late error from an old QEMU process from
// evicting the replacement process's client.
func removeClient(client *QEMU) {
	if client == nil {
		return
	}
	removed := takeClient(client.socketPath, client)
	closeClientAsync(removed)
}

// Remove closes and removes the current client for socketPath.
func Remove(socketPath string) {
	client := takeClient(socketPath, nil)
	closeClientAsync(client)
}

// takeClient removes the current client. When expected is non-nil, removal is
// conditional on pointer identity to protect against socket-path reuse.
func takeClient(socketPath string, expected *QEMU) *QEMU {
	clientPool.Lock()
	defer clientPool.Unlock()

	client, ok := clientPool.clients[socketPath]
	if !ok || (expected != nil && client != expected) {
		return nil
	}
	delete(clientPool.clients, socketPath)
	return client
}

func closeClientAsync(client *QEMU) {
	if client != nil && client.client != nil {
		go client.client.Close()
	}
}
