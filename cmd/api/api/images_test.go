package api

import (
	"fmt"
	"testing"
	"time"

	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListImages_Empty(t *testing.T) {
	svc := newTestService(t)

	resp, err := svc.ListImages(ctx(), oapi.ListImagesRequestObject{})
	require.NoError(t, err)

	list, ok := resp.(oapi.ListImages200JSONResponse)
	require.True(t, ok, "expected 200 response")
	assert.Empty(t, list)
}

func TestGetImage_NotFound(t *testing.T) {
	svc := newTestService(t)

	resp, err := svc.GetImage(ctx(), oapi.GetImageRequestObject{
		Name: "non-existent:latest",
	})
	require.NoError(t, err)

	notFound, ok := resp.(oapi.GetImage404JSONResponse)
	require.True(t, ok, "expected 404 response")
	assert.Equal(t, "not_found", notFound.Code)
	assert.Equal(t, "image not found", notFound.Message)
}

func TestCreateImage_Async(t *testing.T) {
	svc := newTestService(t)
	ctx := ctx()

	// Create images before alpine to populate the queue
	t.Log("Creating image queue...")
	queueImages := []string{
		"docker.io/library/busybox:latest",
		"docker.io/library/nginx:alpine",
	}
	for _, name := range queueImages {
		_, err := svc.CreateImage(ctx, oapi.CreateImageRequestObject{
			Body: &oapi.CreateImageRequest{Name: name},
		})
		require.NoError(t, err)
	}

	// Create alpine (should be last in queue)
	t.Log("Creating alpine image (should be queued)...")
	createResp, err := svc.CreateImage(ctx, oapi.CreateImageRequestObject{
		Body: &oapi.CreateImageRequest{
			Name: "docker.io/library/alpine:latest",
		},
	})
	require.NoError(t, err)

	acceptedResp, ok := createResp.(oapi.CreateImage202JSONResponse)
	require.True(t, ok, "expected 202 accepted response")

	img := oapi.Image(acceptedResp)
	require.Equal(t, "docker.io/library/alpine:latest", img.Name)
	t.Logf("Image created: name=%s, initial_status=%s, queue_position=%v", 
		img.Name, img.Status, img.QueuePosition)

	// Poll until ready
	t.Log("Polling for completion...")
	lastStatus := img.Status
	lastQueuePos := getQueuePos(img.QueuePosition)
	
	for i := 0; i < 3000; i++ {
		getResp, err := svc.GetImage(ctx, oapi.GetImageRequestObject{Name: img.Name})
		require.NoError(t, err)

		imgResp, ok := getResp.(oapi.GetImage200JSONResponse)
		require.True(t, ok, "expected 200 response")

		currentImg := oapi.Image(imgResp)
		currentQueuePos := getQueuePos(currentImg.QueuePosition)
		
		// Log when status or queue position changes
		if currentImg.Status != lastStatus || currentQueuePos != lastQueuePos {
			t.Logf("Update: status=%s, queue_position=%v", currentImg.Status, formatQueuePos(currentImg.QueuePosition))
			
			// Queue position should only decrease (never increase)
			if lastQueuePos > 0 && currentQueuePos > lastQueuePos {
				t.Errorf("Queue position increased: %d -> %d", lastQueuePos, currentQueuePos)
			}
			
			lastStatus = currentImg.Status
			lastQueuePos = currentQueuePos
		}

		if currentImg.Status == oapi.ImageStatus(images.StatusReady) {
			t.Log("Build complete!")
			require.NotNil(t, currentImg.SizeBytes)
			require.Greater(t, *currentImg.SizeBytes, int64(0))
			require.Nil(t, currentImg.Error)
			return
		}

		if currentImg.Status == oapi.ImageStatus(images.StatusFailed) {
			errMsg := ""
			if currentImg.Error != nil {
				errMsg = *currentImg.Error
			}
			t.Fatalf("Build failed: %s", errMsg)
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("Build did not complete within 30 seconds")
}

func TestCreateImage_InvalidTag(t *testing.T) {
	svc := newTestService(t)
	ctx := ctx()

	t.Log("Creating image with invalid tag...")
	createResp, err := svc.CreateImage(ctx, oapi.CreateImageRequestObject{
		Body: &oapi.CreateImageRequest{
			Name: "docker.io/library/busybox:foobar",
		},
	})
	require.NoError(t, err)

	acceptedResp, ok := createResp.(oapi.CreateImage202JSONResponse)
	require.True(t, ok, "expected 202 accepted response")

	img := oapi.Image(acceptedResp)
	require.Equal(t, "docker.io/library/busybox:foobar", img.Name)
	t.Logf("Image created: name=%s", img.Name)

	// Poll until failed
	t.Log("Polling for failure...")
	for i := 0; i < 1000; i++ {
		getResp, err := svc.GetImage(ctx, oapi.GetImageRequestObject{Name: img.Name})
		require.NoError(t, err)

		imgResp, ok := getResp.(oapi.GetImage200JSONResponse)
		require.True(t, ok, "expected 200 response")

		currentImg := oapi.Image(imgResp)
		t.Logf("Status: %s", currentImg.Status)

		if currentImg.Status == oapi.ImageStatus(images.StatusFailed) {
			t.Log("Build failed as expected")
			require.NotNil(t, currentImg.Error)
			require.Contains(t, *currentImg.Error, "foobar")
			t.Logf("Error message: %s", *currentImg.Error)
			return
		}

		if currentImg.Status == oapi.ImageStatus(images.StatusReady) {
			t.Fatal("Build should have failed but succeeded")
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("Build did not fail within timeout")
}

func TestCreateImage_InvalidName(t *testing.T) {
	svc := newTestService(t)
	ctx := ctx()

	invalidNames := []string{
		"invalid::",
		"has spaces",
		"",
	}

	for _, name := range invalidNames {
		t.Run(name, func(t *testing.T) {
			createResp, err := svc.CreateImage(ctx, oapi.CreateImageRequestObject{
				Body: &oapi.CreateImageRequest{Name: name},
			})
			require.NoError(t, err)

			badReq, ok := createResp.(oapi.CreateImage400JSONResponse)
			require.True(t, ok, "expected 400 bad request for invalid name: %s", name)
			require.Equal(t, "invalid_name", badReq.Code)
		})
	}
}

func TestCreateImage_Idempotent(t *testing.T) {
	svc := newTestService(t)
	ctx := ctx()

	// Create first image to occupy queue position 0
	t.Log("Creating first image (busybox) to occupy queue...")
	_, err := svc.CreateImage(ctx, oapi.CreateImageRequestObject{
		Body: &oapi.CreateImageRequest{Name: "docker.io/library/busybox:latest"},
	})
	require.NoError(t, err)

	imageName := "docker.io/library/alpine:3.18"

	// First call - should create and queue at position 1
	t.Log("First CreateImage call (alpine)...")
	resp1, err := svc.CreateImage(ctx, oapi.CreateImageRequestObject{
		Body: &oapi.CreateImageRequest{Name: imageName},
	})
	require.NoError(t, err)
	
	accepted1, ok := resp1.(oapi.CreateImage202JSONResponse)
	require.True(t, ok, "expected 202 response")
	img1 := oapi.Image(accepted1)
	require.Equal(t, imageName, img1.Name)
	require.Equal(t, oapi.ImageStatus(images.StatusPending), img1.Status)
	require.NotNil(t, img1.QueuePosition, "should have queue position")
	require.Equal(t, 1, *img1.QueuePosition, "should be at position 1")
	t.Logf("First call: status=%s, queue_position=%v", img1.Status, formatQueuePos(img1.QueuePosition))

	// Second call immediately - should return existing with same queue position
	t.Log("Second CreateImage call (immediate duplicate)...")
	resp2, err := svc.CreateImage(ctx, oapi.CreateImageRequestObject{
		Body: &oapi.CreateImageRequest{Name: imageName},
	})
	require.NoError(t, err)
	
	accepted2, ok := resp2.(oapi.CreateImage202JSONResponse)
	require.True(t, ok, "expected 202 response")
	img2 := oapi.Image(accepted2)
	require.Equal(t, imageName, img2.Name)
	require.Equal(t, oapi.ImageStatus(images.StatusPending), img2.Status)
	require.NotNil(t, img2.QueuePosition, "should have queue position")
	require.Equal(t, 1, *img2.QueuePosition, "should still be at position 1")
	t.Logf("Second call: status=%s, queue_position=%v", img2.Status, formatQueuePos(img2.QueuePosition))

	// Wait for build to complete
	t.Log("Waiting for build to complete...")
	for i := 0; i < 3000; i++ {
		getResp, err := svc.GetImage(ctx, oapi.GetImageRequestObject{Name: imageName})
		require.NoError(t, err)

		imgResp, ok := getResp.(oapi.GetImage200JSONResponse)
		require.True(t, ok, "expected 200 response")

		currentImg := oapi.Image(imgResp)
		
		if currentImg.Status == oapi.ImageStatus(images.StatusReady) {
			t.Log("Build complete!")
			break
		}

		if currentImg.Status == oapi.ImageStatus(images.StatusFailed) {
			errMsg := ""
			if currentImg.Error != nil {
				errMsg = *currentImg.Error
			}
			t.Fatalf("Build failed: %s", errMsg)
		}

		time.Sleep(10 * time.Millisecond)
	}

	// Third call after completion - should return ready image with no queue position
	t.Log("Third CreateImage call (after completion)...")
	resp3, err := svc.CreateImage(ctx, oapi.CreateImageRequestObject{
		Body: &oapi.CreateImageRequest{Name: imageName},
	})
	require.NoError(t, err)
	
	accepted3, ok := resp3.(oapi.CreateImage202JSONResponse)
	require.True(t, ok, "expected 202 response")
	img3 := oapi.Image(accepted3)
	require.Equal(t, imageName, img3.Name)
	require.Equal(t, oapi.ImageStatus(images.StatusReady), img3.Status, "should return ready image")
	require.Nil(t, img3.QueuePosition, "ready image should have no queue position")
	require.NotNil(t, img3.SizeBytes)
	require.Greater(t, *img3.SizeBytes, int64(0))
	t.Logf("Third call: status=%s, queue_position=%v, size=%d", 
		img3.Status, formatQueuePos(img3.QueuePosition), *img3.SizeBytes)
	
	t.Log("Idempotency test passed!")
}

func getQueuePos(pos *int) int {
	if pos == nil {
		return 0
	}
	return *pos
}

func formatQueuePos(pos *int) string {
	if pos == nil {
		return "none"
	}
	return fmt.Sprintf("%d", *pos)
}


