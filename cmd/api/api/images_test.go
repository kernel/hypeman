package api

import (
	"bufio"
	"encoding/json"
	"strings"
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

func TestCreateImage_AsyncWithSSE(t *testing.T) {
	svc := newTestService(t)
	ctx := ctx()

	// 1. Create image (should return 202 Accepted immediately)
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
	require.Contains(t, []oapi.ImageStatus{images.StatusPending, images.StatusPulling}, img.Status)
	require.Equal(t, 0, img.Progress)

	// 2. Stream progress via SSE
	progressResp, err := svc.GetImageProgress(ctx, oapi.GetImageProgressRequestObject{
		Id: img.Id,
	})
	require.NoError(t, err)

	sseResp, ok := progressResp.(oapi.GetImageProgress200TexteventStreamResponse)
	if !ok {
		t.Fatalf("expected SSE stream, got %T", progressResp)
	}

	// Read SSE events
	scanner := bufio.NewScanner(sseResp.Body)
	lastProgress := 0
	sawPulling := false
	sawUnpacking := false
	sawConverting := false

	timeout := time.After(3 * time.Minute)
	done := make(chan bool)

	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}

			data := strings.TrimPrefix(line, "data: ")
			var update images.ProgressUpdate
			if err := json.Unmarshal([]byte(data), &update); err != nil {
				continue
			}

			t.Logf("SSE: status=%s, progress=%d%%", update.Status, update.Progress)

			// Track which phases we see
			if update.Status == images.StatusPulling {
				sawPulling = true
			}
			if update.Status == images.StatusUnpacking {
				sawUnpacking = true
			}
			if update.Status == images.StatusConverting {
				sawConverting = true
			}

			// Progress should be monotonic
			require.GreaterOrEqual(t, update.Progress, lastProgress)
			lastProgress = update.Progress

			// Stop when ready
			if update.Status == images.StatusReady {
				require.Equal(t, 100, update.Progress)
				done <- true
				return
			}

			// Fail on error
			if update.Status == images.StatusFailed {
				t.Fatalf("Build failed: %v", update.Error)
			}
		}
	}()

	// Wait for completion or timeout
	select {
	case <-done:
		// Success
	case <-timeout:
		t.Fatal("Build did not complete within 3 minutes")
	}

	// Verify we saw at least one intermediate phase (build might be too fast to catch all)
	sawAnyPhase := sawPulling || sawUnpacking || sawConverting
	require.True(t, sawAnyPhase || lastProgress == 100, "should see at least one build phase or final state")

	// 3. Verify final image state
	getResp, err := svc.GetImage(ctx, oapi.GetImageRequestObject{Id: img.Id})
	require.NoError(t, err)

	imgResp, ok := getResp.(oapi.GetImage200JSONResponse)
	require.True(t, ok, "expected 200 response")

	finalImg := oapi.Image(imgResp)
	require.Equal(t, oapi.ImageStatus(images.StatusReady), finalImg.Status)
	require.Equal(t, 100, finalImg.Progress)
	require.NotNil(t, finalImg.SizeBytes)
	require.Greater(t, *finalImg.SizeBytes, int64(0))
	require.Nil(t, finalImg.QueuePosition)
	require.Nil(t, finalImg.Error)
}

