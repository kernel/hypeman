package instances

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/stretchr/testify/require"
)

// TestExecRapidSequential tests rapid sequential exec commands.
// This catches timing/concurrency issues in the exec infrastructure.
func TestExecRapidSequential(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Fatal("/dev/kvm not available")
	}

	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	manager, tmpDir := setupTestManager(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	// Setup image
	imageManager, err := images.NewManager(p, 1)
	require.NoError(t, err)

	t.Log("Pulling nginx:alpine image...")
	_, err = imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/nginx:alpine",
	})
	require.NoError(t, err)

	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, "docker.io/library/nginx:alpine")
		if err == nil && img.Status == images.StatusReady {
			break
		}
		time.Sleep(1 * time.Second)
	}
	t.Log("Image ready")

	// Ensure system files
	systemManager := system.NewManager(p)
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)

	// Create nginx instance
	t.Log("Creating nginx instance...")
	inst, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "exec-test",
		Image:          "docker.io/library/nginx:alpine",
		Size:           512 * 1024 * 1024,
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
	})
	require.NoError(t, err)
	t.Logf("Instance created: %s", inst.Id)

	t.Cleanup(func() {
		t.Log("Cleaning up...")
		manager.DeleteInstance(ctx, inst.Id)
	})

	// Wait for exec-agent
	err = waitForExecAgent(ctx, manager, inst.Id, 15*time.Second)
	require.NoError(t, err, "exec-agent should be ready")

	// Run rapid sequential exec commands
	t.Log("Running rapid sequential exec commands...")
	for i := 1; i <= 10; i++ {
		// Write
		writeCmd := fmt.Sprintf("echo '%d' > /tmp/test.txt", i)
		output, code, err := execWithRetry(ctx, inst.VsockSocket, []string{"/bin/sh", "-c", writeCmd})
		require.NoError(t, err, "write %d should not error", i)
		require.Equal(t, 0, code, "write %d should succeed, output: %s", i, output)

		// Read
		output, code, err = execWithRetry(ctx, inst.VsockSocket, []string{"cat", "/tmp/test.txt"})
		require.NoError(t, err, "read %d should not error", i)
		require.Equal(t, 0, code, "read %d should succeed", i)

		expected := fmt.Sprintf("%d", i)
		actual := strings.TrimSpace(output)
		require.Equal(t, expected, actual, "iteration %d: expected %q, got %q", i, expected, actual)

		t.Logf("Iteration %d: wrote and read %q successfully", i, expected)
	}

	t.Log("All 10 iterations passed!")
}

