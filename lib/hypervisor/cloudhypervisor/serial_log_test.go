package cloudhypervisor

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSerialSocketLoggerWritesWithAppendAfterTruncate(t *testing.T) {
	tmp := socketTempDir(t)
	socketPath := filepath.Join(tmp, "serial.sock")
	logPath := filepath.Join(tmp, "logs", "app.log")

	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	logger, err := startSerialSocketLogger(t.Context(), socketPath, logPath)
	require.NoError(t, err)
	defer logger.Close()

	conn, err := listener.Accept()
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.Write([]byte("first\n"))
	require.NoError(t, err)
	requireEventuallyFileContent(t, logPath, "first\n")

	require.NoError(t, os.Truncate(logPath, 0))
	_, err = conn.Write([]byte("second\n"))
	require.NoError(t, err)
	requireEventuallyFileContent(t, logPath, "second\n")
}

func TestDialSerialSocketRetriesUntilAvailable(t *testing.T) {
	tmp := socketTempDir(t)
	socketPath := filepath.Join(tmp, "serial.sock")

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	connected := make(chan net.Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := dialSerialSocket(ctx, socketPath)
		if err != nil {
			errCh <- err
			return
		}
		connected <- conn
	}()

	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	serverConn, err := listener.Accept()
	require.NoError(t, err)
	defer serverConn.Close()

	select {
	case conn := <-connected:
		defer conn.Close()
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for serial socket dial")
	}
}

func TestSerialSocketLoggerCloseIsIdempotent(t *testing.T) {
	tmp := socketTempDir(t)
	socketPath := filepath.Join(tmp, "serial.sock")
	logPath := filepath.Join(tmp, "logs", "app.log")

	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	logger, err := startSerialSocketLogger(t.Context(), socketPath, logPath)
	require.NoError(t, err)

	conn, err := listener.Accept()
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.Write([]byte("before-close\n"))
	require.NoError(t, err)
	requireEventuallyFileContent(t, logPath, "before-close\n")

	logger.Close()
	logger.Close()

	select {
	case <-logger.done:
	case <-time.After(time.Second):
		t.Fatal("serial logger did not stop after Close")
	}
}

func TestSerialLogPathsFromSnapshot(t *testing.T) {
	tmp := t.TempDir()
	socketPath := filepath.Join("/var/lib/hypeman/guests/test", cloudHypervisorSerialSocketName)
	data := []byte(`{"serial":{"mode":"Socket","socket":"` + socketPath + `"}}`)
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "config.json"), data, 0644))

	gotSocket, gotLog := serialLogPathsFromSnapshot(tmp)
	require.Equal(t, socketPath, gotSocket)
	require.Equal(t, "/var/lib/hypeman/guests/test/logs/app.log", gotLog)
}

func TestSerialLogPathsFromSnapshotIgnoresUnsupportedConfig(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "file mode", data: `{"serial":{"mode":"File","file":"/tmp/app.log"}}`},
		{name: "missing socket", data: `{"serial":{"mode":"Socket"}}`},
		{name: "invalid json", data: `{`},
		{name: "missing serial", data: `{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(tmp, "config.json"), []byte(tt.data), 0644))

			gotSocket, gotLog := serialLogPathsFromSnapshot(tmp)
			require.Empty(t, gotSocket)
			require.Empty(t, gotLog)
		})
	}
}

func TestRemoveStaleSerialSocket(t *testing.T) {
	tmp := socketTempDir(t)
	socketPath := filepath.Join(tmp, "serial.sock")

	require.NoError(t, os.WriteFile(socketPath, []byte("stale"), 0644))
	require.FileExists(t, socketPath)

	require.NoError(t, removeStaleSerialSocket(socketPath))
	require.NoFileExists(t, socketPath)
	require.NoError(t, removeStaleSerialSocket(socketPath))
}

func socketTempDir(t *testing.T) string {
	t.Helper()
	tmp, err := os.MkdirTemp("/tmp", "chlog-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	return tmp
}

func requireEventuallyFileContent(t *testing.T, path, want string) {
	t.Helper()
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(path)
		return err == nil && string(data) == want
	}, time.Second, 10*time.Millisecond)
}
