package instances

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/stretchr/testify/require"
)

// waitForExecAgent polls until exec-agent is ready
func waitForExecAgent(ctx context.Context, mgr *manager, instanceID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		logs, err := collectLogs(ctx, mgr, instanceID, 100)
		if err == nil && strings.Contains(logs, "[exec-agent] listening on vsock port 2222") {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return context.DeadlineExceeded
}

// Note: execCommand is defined in network_test.go

// TestExecConcurrent tests concurrent exec commands from multiple goroutines.
// This validates that the exec infrastructure handles concurrent access correctly.
func TestExecConcurrent(t *testing.T) {
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
	imageManager, err := images.NewManager(p, 1, nil)
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
		Vcpus:          2, // More vCPUs for concurrency
		NetworkEnabled: false,
	})
	require.NoError(t, err)
	t.Logf("Instance created: %s", inst.Id)

	t.Cleanup(func() {
		t.Log("Cleaning up...")
		manager.DeleteInstance(ctx, inst.Id)
	})

	// Wait for exec-agent to be ready (retry here is OK - we're just waiting for startup)
	err = waitForExecAgent(ctx, manager, inst.Id, 15*time.Second)
	require.NoError(t, err, "exec-agent should be ready")

	// Verify exec-agent works with a simple command first
	_, code, err := execCommand(ctx, inst.VsockSocket, "echo", "ready")
	require.NoError(t, err, "initial exec should work")
	require.Equal(t, 0, code, "initial exec should succeed")

	// Run 5 concurrent workers, each doing 25 iterations with its own file
	// NO RETRIES - this tests that exec works reliably under concurrent load
	const numWorkers = 5
	const numIterations = 25

	t.Logf("Running %d concurrent workers, %d iterations each (no retries)...", numWorkers, numIterations)

	var wg sync.WaitGroup
	errors := make(chan error, numWorkers)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			filename := fmt.Sprintf("/tmp/test%d.txt", workerID)

			for i := 1; i <= numIterations; i++ {
				// Write (no retry - must work first time)
				writeCmd := fmt.Sprintf("echo '%d-%d' > %s", workerID, i, filename)
				output, code, err := execCommand(ctx, inst.VsockSocket, "/bin/sh", "-c", writeCmd)
				if err != nil {
					errors <- fmt.Errorf("worker %d, iter %d: write error: %w", workerID, i, err)
					return
				}
				if code != 0 {
					errors <- fmt.Errorf("worker %d, iter %d: write failed with code %d, output: %s", workerID, i, code, output)
					return
				}

				// Read (no retry - must work first time)
				output, code, err = execCommand(ctx, inst.VsockSocket, "cat", filename)
				if err != nil {
					errors <- fmt.Errorf("worker %d, iter %d: read error: %w", workerID, i, err)
					return
				}
				if code != 0 {
					errors <- fmt.Errorf("worker %d, iter %d: read failed with code %d", workerID, i, code)
					return
				}

				expected := fmt.Sprintf("%d-%d", workerID, i)
				actual := strings.TrimSpace(output)
				if expected != actual {
					errors <- fmt.Errorf("worker %d, iter %d: expected %q, got %q", workerID, i, expected, actual)
					return
				}
			}
			t.Logf("Worker %d completed %d iterations", workerID, numIterations)
		}(w)
	}

	// Wait for all workers
	wg.Wait()
	close(errors)

	// Check for errors
	var errs []error
	for err := range errors {
		errs = append(errs, err)
	}
	require.Empty(t, errs, "concurrent exec failed: %v", errs)

	t.Logf("All %d workers completed %d iterations each (total: %d exec pairs)", numWorkers, numIterations, numWorkers*numIterations*2)

	// Phase 2: Test long-running concurrent streams
	// This verifies streams don't block each other (e.g., multiple shells or streaming commands)
	t.Log("Phase 2: Testing long-running concurrent streams...")

	const streamWorkers = 5
	const streamDuration = 2 // seconds

	var streamWg sync.WaitGroup
	streamErrors := make(chan error, streamWorkers)
	streamStart := time.Now()

	for w := 0; w < streamWorkers; w++ {
		streamWg.Add(1)
		go func(workerID int) {
			defer streamWg.Done()

			// Command that takes ~2 seconds and produces output
			cmd := fmt.Sprintf("sleep %d && echo 'stream-%d-done'", streamDuration, workerID)
			output, code, err := execCommand(ctx, inst.VsockSocket, "/bin/sh", "-c", cmd)
			if err != nil {
				streamErrors <- fmt.Errorf("stream worker %d: error: %w", workerID, err)
				return
			}
			if code != 0 {
				streamErrors <- fmt.Errorf("stream worker %d: exit code %d", workerID, code)
				return
			}
			expected := fmt.Sprintf("stream-%d-done", workerID)
			if !strings.Contains(output, expected) {
				streamErrors <- fmt.Errorf("stream worker %d: expected %q in output, got %q", workerID, expected, output)
				return
			}
		}(w)
	}

	streamWg.Wait()
	close(streamErrors)

	streamElapsed := time.Since(streamStart)

	// Check for errors
	var streamErrs []error
	for err := range streamErrors {
		streamErrs = append(streamErrs, err)
	}
	require.Empty(t, streamErrs, "long-running streams failed: %v", streamErrs)

	// If concurrent, should complete in ~2-4s; if serialized would be ~10s
	maxExpected := time.Duration(streamDuration+2) * time.Second
	require.Less(t, streamElapsed, maxExpected,
		"streams appear serialized - took %v, expected < %v", streamElapsed, maxExpected)

	t.Logf("Long-running streams completed in %v (concurrent OK)", streamElapsed)
}
