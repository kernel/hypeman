package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createImageErrManager is a fake images.Manager whose CreateImage returns a
// preset error, used to assert the handler maps typed errors to HTTP statuses.
// The embedded interface satisfies the methods the handler doesn't call.
type createImageErrManager struct {
	images.Manager
	err error
}

func (m createImageErrManager) CreateImage(context.Context, images.CreateImageRequest) (*images.Image, error) {
	return nil, m.err
}

type captureCreateImageManager struct {
	images.Manager
	req images.CreateImageRequest
}

func (m *captureCreateImageManager) CreateImage(_ context.Context, req images.CreateImageRequest) (*images.Image, error) {
	m.req = req
	return &images.Image{Name: req.Name, Digest: "sha256:test", Status: images.StatusPending, CreatedAt: time.Now()}, nil
}

func TestCreateImage_MapsBorrowedCredentials(t *testing.T) {
	t.Parallel()

	username, password, token := "borrower", "secret", "registry-token"
	manager := &captureCreateImageManager{}
	svc := &ApiService{ImageManager: manager}

	resp, err := svc.CreateImage(context.Background(), oapi.CreateImageRequestObject{Body: &oapi.CreateImageRequest{
		Name: "registry.example/private/image:latest",
		Credentials: &oapi.PushCredentials{
			Username:      &username,
			Password:      &password,
			RegistryToken: &token,
		},
	}})
	require.NoError(t, err)
	require.IsType(t, oapi.CreateImage202JSONResponse{}, resp)
	require.NotNil(t, manager.req.Credentials)
	assert.Equal(t, username, manager.req.Credentials.Username)
	assert.Equal(t, password, manager.req.Credentials.Password)
	assert.Equal(t, token, manager.req.Credentials.RegistryToken)
}

func TestCreateImage_EmptyCredentialsUseServerKeychain(t *testing.T) {
	t.Parallel()

	manager := &captureCreateImageManager{}
	svc := &ApiService{ImageManager: manager}
	_, err := svc.CreateImage(context.Background(), oapi.CreateImageRequestObject{Body: &oapi.CreateImageRequest{
		Name:        "docker.io/library/alpine:latest",
		Credentials: &oapi.PushCredentials{},
	}})
	require.NoError(t, err)
	assert.Nil(t, manager.req.Credentials)
}

func TestCreateImage_ErrorStatusMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		err      error
		wantType any
		wantCode string
	}{
		{
			name:     "platform not available -> 404",
			err:      fmt.Errorf("resolve: %w", images.ErrPlatformNotAvailable),
			wantType: oapi.CreateImage404JSONResponse{},
			wantCode: "platform_not_available",
		},
		{
			name:     "rate limited -> 429",
			err:      fmt.Errorf("resolve: %w", images.ErrRateLimited),
			wantType: oapi.CreateImage429JSONResponse{},
			wantCode: "rate_limited",
		},
		{
			name:     "not found -> 404",
			err:      fmt.Errorf("resolve: %w", images.ErrNotFound),
			wantType: oapi.CreateImage404JSONResponse{},
			wantCode: "not_found",
		},
		{
			name:     "invalid platform -> 400",
			err:      fmt.Errorf("resolve: %w", images.ErrInvalidPlatform),
			wantType: oapi.CreateImage400JSONResponse{},
			wantCode: "invalid_platform",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := &ApiService{ImageManager: createImageErrManager{err: tc.err}}

			resp, err := svc.CreateImage(ctx(), oapi.CreateImageRequestObject{
				Body: &oapi.CreateImageRequest{Name: "docker.io/library/alpine:3.19"},
			})
			require.NoError(t, err)
			require.IsType(t, tc.wantType, resp)
			require.Equal(t, tc.wantCode, errorCodeOf(resp))
		})
	}
}

// errorCodeOf extracts the Code field from any CreateImage error response.
func errorCodeOf(resp oapi.CreateImageResponseObject) string {
	switch r := resp.(type) {
	case oapi.CreateImage400JSONResponse:
		return r.Code
	case oapi.CreateImage404JSONResponse:
		return r.Code
	case oapi.CreateImage429JSONResponse:
		return r.Code
	case oapi.CreateImage500JSONResponse:
		return r.Code
	default:
		return ""
	}
}

func TestListImages_Empty(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	resp, err := svc.ListImages(ctx(), oapi.ListImagesRequestObject{})
	require.NoError(t, err)

	list, ok := resp.(oapi.ListImages200JSONResponse)
	require.True(t, ok, "expected 200 response")
	assert.Empty(t, list)
}

func TestListImages_FilterByTagsIncludesDigestOnlyImages(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	const digestRef = "docker.io/library/alpine@sha256:029a752048e32e843bd6defe3841186fb8d19a28dae8ec287f433bb9d6d1ad85"
	seedReadyDigestOnlyImage(t, svc, digestRef, map[string]string{
		"qa":      "pr43-qa-20260323134902",
		"surface": "image",
	})

	getResp, err := svc.GetImage(ctxWithImage(svc, digestRef), oapi.GetImageRequestObject{Name: digestRef})
	require.NoError(t, err)

	gotImage, ok := getResp.(oapi.GetImage200JSONResponse)
	require.True(t, ok, "expected 200 response")
	require.NotNil(t, gotImage.Tags)
	require.Equal(t, "pr43-qa-20260323134902", (*gotImage.Tags)["qa"])

	filter := oapi.Tags{
		"qa": "pr43-qa-20260323134902",
	}
	listResp, err := svc.ListImages(ctx(), oapi.ListImagesRequestObject{
		Params: oapi.ListImagesParams{Tags: &filter},
	})
	require.NoError(t, err)

	list, ok := listResp.(oapi.ListImages200JSONResponse)
	require.True(t, ok, "expected 200 response")
	require.Len(t, list, 1, "digest-only images with matching tags should be listed")
	require.Equal(t, digestRef, list[0].Name)
}

func TestGetImage_NotFound(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	// With middleware, not-found would be handled before reaching handler.
	// For this test, we call the manager directly to verify the error.
	_, err := svc.ImageManager.GetImage(ctx(), "non-existent:latest")
	require.Error(t, err)
}

func TestDeleteImage_DigestOnlyImageDoesNotInternalError(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	const digestRef = "docker.io/library/alpine@sha256:029a752048e32e843bd6defe3841186fb8d19a28dae8ec287f433bb9d6d1ad85"
	seedReadyDigestOnlyImage(t, svc, digestRef, map[string]string{
		"qa": "pr43-delete-20260323134902",
	})

	resp, err := svc.DeleteImage(ctxWithImage(svc, digestRef), oapi.DeleteImageRequestObject{Name: digestRef})
	require.NoError(t, err)

	_, ok := resp.(oapi.DeleteImage204Response)
	require.True(t, ok, "expected deleting an existing digest-only image not to return internal_error")
}

func TestCreateImage_Async(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	ctx := ctx()

	// Create images before alpine to populate the queue
	t.Log("Creating image queue...")
	queueImages := []string{
		apiTestImageRef(t, "docker.io/library/busybox:latest"),
		apiTestImageRef(t, "docker.io/library/nginx:alpine"),
	}
	for _, name := range queueImages {
		_, err := svc.CreateImage(ctx, oapi.CreateImageRequestObject{
			Body: &oapi.CreateImageRequest{Name: name},
		})
		require.NoError(t, err)
	}

	// Create alpine (should be last in queue)
	t.Log("Creating alpine image (should be queued)...")
	alpineName := apiTestImageRef(t, "docker.io/library/alpine:latest")
	createResp, err := svc.CreateImage(ctx, oapi.CreateImageRequestObject{
		Body: &oapi.CreateImageRequest{
			Name: alpineName,
		},
	})
	require.NoError(t, err)

	acceptedResp, ok := createResp.(oapi.CreateImage202JSONResponse)
	require.True(t, ok, "expected 202 accepted response")

	img := oapi.Image(acceptedResp)
	require.Equal(t, alpineName, img.Name)
	require.NotEmpty(t, img.Digest, "digest should be populated immediately")
	t.Logf("Image created: name=%s, digest=%s, initial_status=%s, queue_position=%v",
		img.Name, img.Digest, img.Status, img.QueuePosition)

	// Construct digest reference for polling: repository@digest
	// GetImage expects format like "docker.io/library/alpine@sha256:..."
	alpineRef, err := images.ParseNormalizedRef(alpineName)
	require.NoError(t, err)
	digestRef := alpineRef.Repository() + "@" + img.Digest
	t.Logf("Polling with digest reference: %s", digestRef)

	// Poll until ready using digest (tag symlink doesn't exist until status=ready)
	t.Log("Polling for completion...")
	lastStatus := img.Status
	lastQueuePos := getQueuePos(img.QueuePosition)

	for i := 0; i < 3000; i++ {
		getResp, err := svc.GetImage(ctxWithImage(svc, digestRef), oapi.GetImageRequestObject{Name: digestRef})
		require.NoError(t, err)

		imgResp, ok := getResp.(oapi.GetImage200JSONResponse)
		if !ok {
			t.Fatalf("expected 200 response, got %T: %+v", getResp, getResp)
		}

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
	t.Parallel()
	svc := newTestService(t)
	ctx := ctx()

	t.Log("Creating image with invalid tag...")
	createResp, err := svc.CreateImage(ctx, oapi.CreateImageRequestObject{
		Body: &oapi.CreateImageRequest{
			Name: apiTestImageRef(t, "docker.io/library/busybox:foobar"),
		},
	})
	require.NoError(t, err)

	// With go-containerregistry, manifest validation happens synchronously
	// Invalid tags fail immediately with 404 (manifest not found)
	errorResp, ok := createResp.(oapi.CreateImage404JSONResponse)
	require.True(t, ok, "expected 404 not found response for invalid tag")

	errObj := oapi.Error(errorResp)
	require.Equal(t, "not_found", errObj.Code)
	t.Logf("Got expected error: code=%s message=%s", errObj.Code, errObj.Message)
}

func TestCreateImage_InvalidName(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	svc := newTestService(t)
	ctx := ctx()

	// Create first image to occupy queue position 0
	t.Log("Creating first image (nginx) to occupy queue...")
	_, err := svc.CreateImage(ctx, oapi.CreateImageRequestObject{
		Body: &oapi.CreateImageRequest{Name: apiTestImageRef(t, "docker.io/library/nginx:alpine")},
	})
	require.NoError(t, err)

	imageName := apiTestImageRef(t, "docker.io/library/alpine:latest")

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
	require.NotEmpty(t, img1.Digest, "digest should be populated immediately")
	require.Equal(t, oapi.ImageStatus(images.StatusPending), img1.Status)
	require.NotNil(t, img1.QueuePosition, "should have queue position")
	require.Equal(t, 1, *img1.QueuePosition, "should be at position 1")
	t.Logf("First call: name=%s, digest=%s, status=%s, queue_position=%v", img1.Name, img1.Digest, img1.Status, formatQueuePos(img1.QueuePosition))

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
	require.Equal(t, img1.Digest, img2.Digest, "should have same digest")

	// Log actual status to see what's happening
	t.Logf("Second call: digest=%s, status=%s, queue_position=%v, error=%v",
		img2.Digest, img2.Status, formatQueuePos(img2.QueuePosition), img2.Error)

	// If it failed, we need to see why
	if img2.Status == oapi.ImageStatus(images.StatusFailed) {
		if img2.Error != nil {
			t.Logf("Build failed with error: %s", *img2.Error)
		}
		t.Fatal("Build failed - this is the root cause of test failures")
	}

	// Status can be "pending" (still queued), "pulling" (pull started), or "ready" (completed)
	// The key idempotency invariant is that the digest is the same (verified above)
	require.Contains(t, []oapi.ImageStatus{
		oapi.ImageStatus(images.StatusPending),
		oapi.ImageStatus(images.StatusPulling),
		oapi.ImageStatus(images.StatusReady),
	}, img2.Status, "status should be pending, pulling, or ready")

	// If still pending, should have queue position
	if img2.Status == oapi.ImageStatus(images.StatusPending) {
		require.NotNil(t, img2.QueuePosition, "should have queue position when pending")
	}

	// Construct digest reference: repository@digest
	imageRef, err := images.ParseNormalizedRef(imageName)
	require.NoError(t, err)
	digestRef := imageRef.Repository() + "@" + img1.Digest
	t.Logf("Polling with digest reference: %s", digestRef)

	// Wait for build to complete - poll by digest (tag symlink doesn't exist until status=ready)
	t.Log("Waiting for build to complete...")
	for i := 0; i < 3000; i++ {
		getResp, err := svc.GetImage(ctxWithImage(svc, digestRef), oapi.GetImageRequestObject{Name: digestRef})
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

func seedReadyDigestOnlyImage(t *testing.T, svc *ApiService, imageRef string, imageTags map[string]string) {
	t.Helper()

	ref, err := images.ParseNormalizedRef(imageRef)
	require.NoError(t, err)
	require.True(t, ref.IsDigest(), "test helper expects a digest reference")

	p := paths.New(svc.Config.DataDir)
	digestDir := p.ImageDigestDir(ref.Repository(), ref.DigestHex())
	require.NoError(t, os.MkdirAll(digestDir, 0o755))
	require.NoError(t, os.WriteFile(p.ImageDigestPath(ref.Repository(), ref.DigestHex()), []byte("rootfs"), 0o644))

	meta := struct {
		Name      string            `json:"name"`
		Digest    string            `json:"digest"`
		Status    string            `json:"status"`
		SizeBytes int64             `json:"size_bytes"`
		Tags      map[string]string `json:"tags,omitempty"`
		CreatedAt time.Time         `json:"created_at"`
	}{
		Name:      imageRef,
		Digest:    "sha256:" + ref.DigestHex(),
		Status:    "ready",
		SizeBytes: int64(len("rootfs")),
		Tags:      imageTags,
		CreatedAt: time.Now().UTC(),
	}

	data, err := json.Marshal(meta)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p.ImageMetadata(ref.Repository(), ref.DigestHex()), data, 0o644))
}
