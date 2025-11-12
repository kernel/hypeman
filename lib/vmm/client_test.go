package vmm

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/onkernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractBinary(t *testing.T) {
	tmpDir := t.TempDir()

	// Test extraction for v48.0
	binaryPath, err := ExtractBinary(paths.New(tmpDir), V48_0)
	require.NoError(t, err)

	// Verify file exists
	_, err = os.Stat(binaryPath)
	require.NoError(t, err)

	// Verify executable
	info, err := os.Stat(binaryPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0755), info.Mode().Perm())

	// Test idempotency - second extraction should succeed and return same path
	binaryPath2, err := ExtractBinary(paths.New(tmpDir), V48_0)
	require.NoError(t, err)
	assert.Equal(t, binaryPath, binaryPath2)
}

func TestIsVersionSupported(t *testing.T) {
	assert.True(t, IsVersionSupported(V48_0))
	assert.True(t, IsVersionSupported(V49_0))
	assert.False(t, IsVersionSupported("v1.0"))
}

func TestParseVersion(t *testing.T) {
	tmpDir := t.TempDir()

	// Extract binary
	binaryPath, err := ExtractBinary(paths.New(tmpDir), V48_0)
	require.NoError(t, err)

	// Parse version
	version, err := ParseVersion(binaryPath)
	require.NoError(t, err)
	assert.Equal(t, V48_0, version)
}

func TestStartProcessAndShutdown(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")
	ctx := context.Background()

	// Start VMM process
	pid, err := StartProcess(ctx, paths.New(tmpDir), V48_0, socketPath)
	require.NoError(t, err)
	assert.Greater(t, pid, 0, "PID should be positive")

	// Verify socket exists
	_, err = os.Stat(socketPath)
	require.NoError(t, err)

	// Create client
	client, err := NewVMM(socketPath)
	require.NoError(t, err)
	require.NotNil(t, client)

	// Ping the VMM to get PID
	pingResp, err := client.GetVmmPingWithResponse(ctx)
	require.NoError(t, err)
	assert.Equal(t, 200, pingResp.StatusCode())
	require.NotNil(t, pingResp.JSON200)
	require.NotNil(t, pingResp.JSON200.Pid)

	// Shutdown VMM
	shutdownResp, err := client.ShutdownVMMWithResponse(ctx)
	require.NoError(t, err)
	// Note: API spec says 204, but actual implementation returns 200
	assert.True(t, shutdownResp.StatusCode() >= 200 && shutdownResp.StatusCode() < 300,
		"Expected 2xx status code, got %d", shutdownResp.StatusCode())

}

func TestStartProcessSocketInUse(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")
	ctx := context.Background()

	// Start first VMM
	pid, err := StartProcess(ctx, paths.New(tmpDir), V48_0, socketPath)
	require.NoError(t, err)
	assert.Greater(t, pid, 0)

	// Try to start second VMM on same socket - should fail
	_, err = StartProcess(ctx, paths.New(tmpDir), V48_0, socketPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "socket already in use")

	// Cleanup
	client, _ := NewVMM(socketPath)
	if client != nil {
		client.ShutdownVMMWithResponse(ctx)
	}
}

func TestMultipleVersions(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name    string
		version CHVersion
	}{
		{"v48.0", V48_0},
		{"v49.0", V49_0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			socketPath := filepath.Join(tmpDir, tt.name+".sock")
			ctx := context.Background()

			// Start VMM
			pid, err := StartProcess(ctx, paths.New(tmpDir), tt.version, socketPath)
			require.NoError(t, err)
			assert.Greater(t, pid, 0)

			// Create client and ping to get PID
			client, err := NewVMM(socketPath)
			require.NoError(t, err)

			pingResp, err := client.GetVmmPingWithResponse(ctx)
			require.NoError(t, err)
			assert.Equal(t, 200, pingResp.StatusCode())
			require.NotNil(t, pingResp.JSON200)
			require.NotNil(t, pingResp.JSON200.Pid)

			// Shutdown
			shutdownResp, err := client.ShutdownVMMWithResponse(ctx)
			require.NoError(t, err)
			// Note: API spec says 204, but actual implementation returns 200
			assert.True(t, shutdownResp.StatusCode() >= 200 && shutdownResp.StatusCode() < 300,
				"Expected 2xx status code, got %d", shutdownResp.StatusCode())

		})
	}
}

func TestStartProcessCreatesLogFiles(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")
	ctx := context.Background()

	// Start VMM process with verbose logging to ensure output is written
	pid, err := StartProcessWithArgs(ctx, paths.New(tmpDir), V48_0, socketPath, []string{"-v"})
	require.NoError(t, err)
	assert.Greater(t, pid, 0)

	// Verify log files exist - they are created and accessible by the daemon
	stdoutLog := filepath.Join(tmpDir, "ch-stdout.log")
	stderrLog := filepath.Join(tmpDir, "ch-stderr.log")

	_, err = os.Stat(stdoutLog)
	require.NoError(t, err, "stdout log should exist")

	_, err = os.Stat(stderrLog)
	require.NoError(t, err, "stderr log should exist")

	// Verify the daemon is running and responsive
	client, err := NewVMM(socketPath)
	require.NoError(t, err)

	pingResp, err := client.GetVmmPingWithResponse(ctx)
	require.NoError(t, err)
	assert.Equal(t, 200, pingResp.StatusCode())

	// Read log files - with verbose mode, Cloud Hypervisor writes to logs
	stdoutContent, err := os.ReadFile(stdoutLog)
	require.NoError(t, err)
	stderrContent, err := os.ReadFile(stderrLog)
	require.NoError(t, err)

	// Verify that logs contain output (proves daemon can write after parent closed files)
	totalLogSize := len(stdoutContent) + len(stderrContent)
	assert.Greater(t, totalLogSize, 0,
		"Cloud Hypervisor daemon should write logs even after parent closed the file descriptors")

	// Cleanup
	client.ShutdownVMMWithResponse(ctx)
}
