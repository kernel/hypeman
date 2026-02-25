package builds

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/images"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildImageAvailabilityRace demonstrates the race condition described in KERNEL-863:
// After WaitForBuild() returns with BuildStatusReady, the image may not be immediately
// available for instance creation because:
// 1. Registry returns 201 to builder
// 2. Registry calls triggerConversion() asynchronously
// 3. Builder reports success, build status becomes "ready"
// 4. But image conversion may still be in progress
//
// This test simulates the scenario where a build completes but the image
// is not yet ready when the client tries to use it.
func TestBuildImageAvailabilityRace(t *testing.T) {
	// This test demonstrates the conceptual race condition.
	// The actual fix requires changes to either:
	// 1. Wait for image conversion before reporting build success
	// 2. Add an image availability check endpoint
	// 3. Have the builder verify image is pullable before reporting success

	t.Run("demonstrates async conversion race", func(t *testing.T) {
		// Simulate the race: build reports ready but image conversion is async
		var (
			buildReady      = make(chan struct{})
			imageConverted  = make(chan struct{})
			conversionDelay = 100 * time.Millisecond
		)

		// Simulate registry receiving image and starting async conversion
		go func() {
			// Registry returns 201 immediately
			close(buildReady)
			// But conversion happens asynchronously with some delay
			time.Sleep(conversionDelay)
			close(imageConverted)
		}()

		// Simulate client waiting for build to be ready
		<-buildReady

		// Build is "ready" but image might not be converted yet
		select {
		case <-imageConverted:
			// Image already converted - no race in this run
			t.Log("Image was converted before we checked (no race this time)")
		default:
			// This demonstrates the race condition:
			// Build is ready but image is not yet available
			t.Log("RACE CONDITION: Build ready but image not yet converted")

			// In the real system, instance creation would fail here
			// because imageManager.GetImage() would return not found or pending status
		}

		// Wait for conversion to complete
		<-imageConverted
		t.Log("Image conversion completed")
	})
}

// TestWaitForImageReady_Success tests that waitForImageReady succeeds when image becomes ready
func TestWaitForImageReady_Success(t *testing.T) {
	mgr, _, _, imageMgr, tempDir := setupTestManagerWithImageMgr(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	buildID := "test-build-123"

	// Set the image to ready in the mock using the same ref format as production:
	// builds/{id} is what runBuild passes to waitForImageReady
	imageRef := "builds/" + buildID
	imageMgr.SetImageReady(imageRef)

	// waitForImageReady should succeed immediately
	err := mgr.waitForImageReady(ctx, imageRef)
	require.NoError(t, err)
}

// TestWaitForImageReady_WaitsForConversion tests that waitForImageReady polls until ready
func TestWaitForImageReady_WaitsForConversion(t *testing.T) {
	mgr, _, _, imageMgr, tempDir := setupTestManagerWithImageMgr(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	buildID := "test-build-456"
	imageRef := "builds/" + buildID

	// Start with image in pending status
	imageMgr.mu.Lock()
	imageMgr.images[imageRef] = &images.Image{
		Name:   imageRef,
		Status: images.StatusPending,
	}
	imageMgr.mu.Unlock()

	// Simulate conversion completing after a short delay
	go func() {
		time.Sleep(100 * time.Millisecond)
		imageMgr.SetImageStatus(imageRef, images.StatusConverting)
		time.Sleep(100 * time.Millisecond)
		imageMgr.SetImageStatus(imageRef, images.StatusReady)
	}()

	// waitForImageReady should poll and eventually succeed
	start := time.Now()
	err := mgr.waitForImageReady(ctx, imageRef)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.GreaterOrEqual(t, elapsed, 200*time.Millisecond, "should have waited for conversion")
}

// TestWaitForImageReady_Timeout tests that waitForImageReady times out if image never becomes ready
func TestWaitForImageReady_ContextCancelled(t *testing.T) {
	mgr, _, _, imageMgr, tempDir := setupTestManagerWithImageMgr(t)
	defer os.RemoveAll(tempDir)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	buildID := "test-build-789"
	imageRef := "builds/" + buildID

	// Image stays in pending status forever
	imageMgr.mu.Lock()
	imageMgr.images[imageRef] = &images.Image{
		Name:   imageRef,
		Status: images.StatusPending,
	}
	imageMgr.mu.Unlock()

	// waitForImageReady should return context error
	err := mgr.waitForImageReady(ctx, imageRef)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestWaitForImageReady_Failed tests that waitForImageReady returns error if image conversion fails
func TestWaitForImageReady_Failed(t *testing.T) {
	mgr, _, _, imageMgr, tempDir := setupTestManagerWithImageMgr(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	buildID := "test-build-failed"
	imageRef := "builds/" + buildID

	// Image is in failed status
	imageMgr.mu.Lock()
	imageMgr.images[imageRef] = &images.Image{
		Name:   imageRef,
		Status: images.StatusFailed,
	}
	imageMgr.mu.Unlock()

	// waitForImageReady should return error immediately
	err := mgr.waitForImageReady(ctx, imageRef)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image conversion failed")
}

// TestImageAvailabilityAfterBuildComplete tests the proposed fix:
// Build should only report "ready" after verifying the image is available.
func TestImageAvailabilityAfterBuildComplete(t *testing.T) {
	t.Skip("This test is for the proposed fix - not yet implemented")

	// The fix would involve one of:
	//
	// Option 1: Synchronous conversion in registry
	//   - Change `go r.triggerConversion()` to synchronous call
	//   - Pros: Simple fix
	//   - Cons: Increases latency for builder push response
	//
	// Option 2: Builder verifies image availability
	//   - After pushing, builder pulls/verifies the image
	//   - Only then reports success via vsock
	//   - Pros: End-to-end verification
	//   - Cons: Adds complexity to builder agent
	//
	// Option 3: Build manager waits for image
	//   - After receiving success from builder, poll image status
	//   - Only set build to "ready" when image is "ready"
	//   - Pros: Clean separation of concerns
	//   - Cons: Adds polling overhead
	//
	// Option 4: Expose image availability endpoint
	//   - Callers check image availability before creating instances
	//   - Pros: Flexible for callers
	//   - Cons: Pushes complexity to callers (current workaround)
}

// Concurrent access test to verify thread safety of status updates
func TestConcurrentStatusUpdates(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer removeAll(tempDir)

	ctx := context.Background()

	// Create a build
	req := CreateBuildRequest{
		Dockerfile: "FROM alpine",
	}
	build, err := mgr.CreateBuild(ctx, req, []byte("source"))
	require.NoError(t, err)

	// Concurrently subscribe and update status
	var wg sync.WaitGroup
	const numGoroutines = 10

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			// Subscribe
			ch := make(chan BuildEvent, 10)
			mgr.subscribeToStatus(build.ID, ch)
			defer mgr.unsubscribeFromStatus(build.ID, ch)

			// Small delay to interleave
			time.Sleep(time.Duration(id) * time.Millisecond)

			// Read any events
			for {
				select {
				case <-ch:
					// Got event
				case <-time.After(50 * time.Millisecond):
					return
				}
			}
		}(i)
	}

	// Trigger status updates concurrently
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mgr.updateStatus(build.ID, StatusBuilding, nil)
		}()
	}

	wg.Wait()

	// Should not panic or deadlock
	t.Log("Concurrent status updates completed without deadlock")
}

func removeAll(path string) {
	os.RemoveAll(path)
}
