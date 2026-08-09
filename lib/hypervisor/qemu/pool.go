package qemu

import (
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
	clientPool.RLock()
	if client, ok := clientPool.clients[socketPath]; ok && client.hypervisorType == hypervisorType {
		clientPool.RUnlock()
		return client, nil
	}
	clientPool.RUnlock()

	clientPool.Lock()
	if client, ok := clientPool.clients[socketPath]; ok {
		if client.hypervisorType == hypervisorType {
			clientPool.Unlock()
			return client, nil
		}
		delete(clientPool.clients, socketPath)
		clientPool.Unlock()

		// A backend switch needs a fresh client, but a stuck disconnect must not
		// hold the host-wide pool lock. Once it completes, retry so a concurrent
		// creator for this socket can win normally.
		if client.client != nil {
			_ = client.client.Close()
		}
		return GetOrCreateForType(socketPath, hypervisorType)
	}

	client, err := newClient(socketPath, hypervisorType)
	if err != nil {
		clientPool.Unlock()
		return nil, err
	}
	clientPool.clients[socketPath] = client
	clientPool.Unlock()
	return client, nil
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
