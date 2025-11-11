package vmm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractBinary(t *testing.T) {
	tmpDir := t.TempDir()

	// Test extraction for v48.0
	binaryPath, err := ExtractBinary(tmpDir, V48_0)
	require.NoError(t, err)

	// Verify file exists
	_, err = os.Stat(binaryPath)
	require.NoError(t, err)

	// Verify executable
	info, err := os.Stat(binaryPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0755), info.Mode().Perm())

	// Test idempotency - second extraction should succeed and return same path
	binaryPath2, err := ExtractBinary(tmpDir, V48_0)
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
	binaryPath, err := ExtractBinary(tmpDir, V48_0)
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
	err := StartProcess(ctx, tmpDir, V48_0, socketPath)
	require.NoError(t, err)

	// Verify socket exists
	_, err = os.Stat(socketPath)
	require.NoError(t, err)

	// Create client
	client, err := NewVMM(socketPath)
	require.NoError(t, err)
	require.NotNil(t, client)

	// Ping the VMM
	pingResp, err := client.GetVmmPingWithResponse(ctx)
	require.NoError(t, err)
	assert.Equal(t, 200, pingResp.StatusCode())

	// Shutdown VMM
	shutdownResp, err := client.ShutdownVMMWithResponse(ctx)
	require.NoError(t, err)
	// Note: API spec says 204, but actual implementation returns 200
	assert.True(t, shutdownResp.StatusCode() >= 200 && shutdownResp.StatusCode() < 300,
		"Expected 2xx status code, got %d", shutdownResp.StatusCode())

	// Wait a moment for VMM to actually shut down
	time.Sleep(100 * time.Millisecond)
}

func TestStartProcessSocketInUse(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")
	ctx := context.Background()

	// Start first VMM
	err := StartProcess(ctx, tmpDir, V48_0, socketPath)
	require.NoError(t, err)

	// Try to start second VMM on same socket - should fail
	err = StartProcess(ctx, tmpDir, V48_0, socketPath)
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
			err := StartProcess(ctx, tmpDir, tt.version, socketPath)
			require.NoError(t, err)

			// Create client and ping
			client, err := NewVMM(socketPath)
			require.NoError(t, err)

			pingResp, err := client.GetVmmPingWithResponse(ctx)
			require.NoError(t, err)
			assert.Equal(t, 200, pingResp.StatusCode())

			// Shutdown
			shutdownResp, err := client.ShutdownVMMWithResponse(ctx)
			require.NoError(t, err)
			// Note: API spec says 204, but actual implementation returns 200
			assert.True(t, shutdownResp.StatusCode() >= 200 && shutdownResp.StatusCode() < 300,
				"Expected 2xx status code, got %d", shutdownResp.StatusCode())

			time.Sleep(100 * time.Millisecond)
		})
	}
}
