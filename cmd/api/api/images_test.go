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
		Id: "non-existent",
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
	require.Equal(t, "img-alpine-latest", img.Id)
	t.Logf("Image created: id=%s, initial_status=%s, queue_position=%v", 
		img.Id, img.Status, img.QueuePosition)

	// Poll until ready
	t.Log("Polling for completion...")
	lastStatus := img.Status
	lastQueuePos := getQueuePos(img.QueuePosition)
	
	for i := 0; i < 3000; i++ {
		getResp, err := svc.GetImage(ctx, oapi.GetImageRequestObject{Id: img.Id})
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
	t.Logf("Image created: id=%s", img.Id)

	// Poll until failed
	t.Log("Polling for failure...")
	for i := 0; i < 1000; i++ {
		getResp, err := svc.GetImage(ctx, oapi.GetImageRequestObject{Id: img.Id})
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


