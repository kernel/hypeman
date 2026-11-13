package qemu

import (
	"bufio"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestReleaseVMMStateClosesPooledConnection(t *testing.T) {
	for _, hvType := range []hypervisor.Type{hypervisor.TypeQEMU, hypervisor.TypeQEMUMicroVM} {
		t.Run(string(hvType), func(t *testing.T) {
			// t.TempDir() overflows the unix socket path limit on macOS.
			dir, err := os.MkdirTemp("/tmp", "qmp")
			require.NoError(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			socketPath := filepath.Join(dir, "qemu.sock")
			t.Cleanup(func() { releaseVMMState(socketPath) })
			closed := serveFakeQMP(t, socketPath)

			_, err = GetOrCreateForType(socketPath, hvType)
			require.NoError(t, err)
			balloonTargetCache.Store(socketPath, 512<<20)

			hypervisor.ReleaseVMMState(hvType, socketPath)

			clientPool.RLock()
			_, pooled := clientPool.clients[socketPath]
			clientPool.RUnlock()
			require.False(t, pooled)
			_, cached := balloonTargetCache.Load(socketPath)
			require.False(t, cached, "balloon target must not outlive the VM")
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("released client did not close its QMP connection")
			}
		})
	}
}

// serveFakeQMP accepts one QMP connection on socketPath, completes the
// capabilities handshake, and closes the returned channel when the client
// disconnects.
func serveFakeQMP(t *testing.T, socketPath string) <-chan struct{} {
	t.Helper()
	ln, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, `{"QMP":{"version":{"qemu":{"major":8,"minor":2,"micro":0},"package":""},"capabilities":[]}}`+"\n")
		if _, err := bufio.NewReader(conn).ReadBytes('\n'); err != nil {
			return
		}
		_, _ = io.WriteString(conn, `{"return":{}}`+"\n")
		_, _ = io.Copy(io.Discard, conn)
	}()
	return closed
}
