package images

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/onkernel/hypeman/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestCreateImage(t *testing.T) {
	ociClient, err := NewOCIClient(t.TempDir())
	require.NoError(t, err)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, ociClient, 1)

	ctx := context.Background()
	req := oapi.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	// CreateImage returns immediately with pending status
	img, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, img)
	require.Equal(t, "docker.io/library/alpine:latest", img.Name)
	require.Equal(t, "img-alpine-latest", img.Id)
	require.Contains(t, []oapi.ImageStatus{StatusPending, StatusPulling}, img.Status)

	// Wait for build to complete via progress channel
	progressCh, err := mgr.GetProgress(ctx, img.Id)
	require.NoError(t, err)

	for update := range progressCh {
		if update.Status == StatusReady || update.Status == StatusFailed {
			require.Equal(t, StatusReady, update.Status)
			break
		}
	}

	// Verify final state
	img, err = mgr.GetImage(ctx, img.Id)
	require.NoError(t, err)
	require.Equal(t, oapi.ImageStatus(StatusReady), img.Status)
	require.Equal(t, 100, img.Progress)
	require.NotNil(t, img.SizeBytes)
	require.Greater(t, *img.SizeBytes, int64(0))

	// Verify disk image was created
	diskPath := filepath.Join(dataDir, "images", img.Id, "rootfs.ext4")
	_, err = os.Stat(diskPath)
	require.NoError(t, err)
}

func TestCreateImageWithCustomID(t *testing.T) {
	ociClient, err := NewOCIClient(t.TempDir())
	require.NoError(t, err)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, ociClient, 1)

	ctx := context.Background()
	customID := "my-custom-alpine"
	req := oapi.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
		Id:   &customID,
	}

	img, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, img)
	require.Equal(t, "my-custom-alpine", img.Id)

	// Wait for build to complete
	waitForReady(t, mgr, ctx, img.Id)
}

func TestCreateImageDuplicate(t *testing.T) {
	ociClient, err := NewOCIClient(t.TempDir())
	require.NoError(t, err)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, ociClient, 1)

	ctx := context.Background()
	req := oapi.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	// Create first image
	img1, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)

	// Wait for build to start (moves from pending to pulling)
	waitForReady(t, mgr, ctx, img1.Id)

	// Try to create duplicate (should fail even if first still building)
	_, err = mgr.CreateImage(ctx, req)
	require.ErrorIs(t, err, ErrAlreadyExists)
}

func TestListImages(t *testing.T) {
	ociClient, err := NewOCIClient(t.TempDir())
	require.NoError(t, err)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, ociClient, 1)

	ctx := context.Background()

	// Initially empty
	images, err := mgr.ListImages(ctx)
	require.NoError(t, err)
	require.Len(t, images, 0)

	// Create first image
	req1 := oapi.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}
	img1, err := mgr.CreateImage(ctx, req1)
	require.NoError(t, err)

	// Wait for build
	waitForReady(t, mgr, ctx, img1.Id)

	// List should return one image
	images, err = mgr.ListImages(ctx)
	require.NoError(t, err)
	require.Len(t, images, 1)
	require.Equal(t, "docker.io/library/alpine:latest", images[0].Name)
	require.Equal(t, oapi.ImageStatus(StatusReady), images[0].Status)
}

func TestGetImage(t *testing.T) {
	ociClient, err := NewOCIClient(t.TempDir())
	require.NoError(t, err)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, ociClient, 1)

	ctx := context.Background()
	req := oapi.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	created, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)

	// Wait for build
	waitForReady(t, mgr, ctx, created.Id)

	// Get the image
	img, err := mgr.GetImage(ctx, created.Id)
	require.NoError(t, err)
	require.NotNil(t, img)
	require.Equal(t, created.Id, img.Id)
	require.Equal(t, created.Name, img.Name)
	require.Equal(t, oapi.ImageStatus(StatusReady), img.Status)
	require.NotNil(t, img.SizeBytes)
}

func TestGetImageNotFound(t *testing.T) {
	ociClient, err := NewOCIClient(t.TempDir())
	require.NoError(t, err)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, ociClient, 1)

	ctx := context.Background()

	// Try to get non-existent image
	_, err = mgr.GetImage(ctx, "nonexistent")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestDeleteImage(t *testing.T) {
	ociClient, err := NewOCIClient(t.TempDir())
	require.NoError(t, err)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, ociClient, 1)

	ctx := context.Background()
	req := oapi.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	created, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)

	// Wait for ready
	waitForReady(t, mgr, ctx, created.Id)

	// Delete the image
	err = mgr.DeleteImage(ctx, created.Id)
	require.NoError(t, err)

	// Verify it's gone
	_, err = mgr.GetImage(ctx, created.Id)
	require.ErrorIs(t, err, ErrNotFound)

	// Verify files were removed
	imageDir := filepath.Join(dataDir, "images", created.Id)
	_, err = os.Stat(imageDir)
	require.True(t, os.IsNotExist(err))
}

func TestDeleteImageNotFound(t *testing.T) {
	ociClient, err := NewOCIClient(t.TempDir())
	require.NoError(t, err)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, ociClient, 1)

	ctx := context.Background()

	// Try to delete non-existent image
	err = mgr.DeleteImage(ctx, "nonexistent")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestGenerateImageID(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"docker.io/library/nginx:latest", "img-nginx-latest"},
		{"docker.io/library/alpine:3.18", "img-alpine-3-18"},
		{"gcr.io/my-project/my-app:v1.0.0", "img-my-app-v1-0-0"},
		{"nginx", "img-nginx"},
		{"ubuntu:22.04", "img-ubuntu-22-04"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := generateImageID(tt.input)
			require.Equal(t, tt.expected, result)
		})
	}
}

// waitForReady waits for an image build to complete
func waitForReady(t *testing.T, mgr Manager, ctx context.Context, imageID string) {
	progressCh, err := mgr.GetProgress(ctx, imageID)
	require.NoError(t, err)

	for update := range progressCh {
		t.Logf("Progress: status=%s, progress=%d%%", update.Status, update.Progress)
		if update.Status == StatusReady || update.Status == StatusFailed {
			if update.Status == StatusFailed {
				t.Fatalf("Build failed: %v", update.Error)
			}
			break
		}
	}
}


