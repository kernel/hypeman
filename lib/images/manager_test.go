package images

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCreateImage(t *testing.T) {
	ociClient, err := NewOCIClient(t.TempDir())
	require.NoError(t, err)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, ociClient, 1)

	ctx := context.Background()
	req := CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	img, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, img)
	require.Equal(t, "docker.io/library/alpine:latest", img.Name)

	waitForReady(t, mgr, ctx, img.Name)

	img, err = mgr.GetImage(ctx, img.Name)
	require.NoError(t, err)
	require.Equal(t, StatusReady, img.Status)
	require.NotNil(t, img.SizeBytes)
	require.Greater(t, *img.SizeBytes, int64(0))

	diskPath := filepath.Join(dataDir, "images", imageNameToPath(img.Name), "rootfs.erofs")
	_, err = os.Stat(diskPath)
	require.NoError(t, err)
}

func TestCreateImageDifferentTag(t *testing.T) {
	ociClient, err := NewOCIClient(t.TempDir())
	require.NoError(t, err)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, ociClient, 1)

	ctx := context.Background()
	req := CreateImageRequest{
		Name: "docker.io/library/alpine:3.18",
	}

	img, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, img)
	require.Equal(t, "docker.io/library/alpine:3.18", img.Name)

	waitForReady(t, mgr, ctx, img.Name)
}

func TestCreateImageDuplicate(t *testing.T) {
	ociClient, err := NewOCIClient(t.TempDir())
	require.NoError(t, err)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, ociClient, 1)

	ctx := context.Background()
	req := CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	img1, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)

	waitForReady(t, mgr, ctx, img1.Name)

	// Second create should be idempotent and return existing image
	img2, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, img2)
	require.Equal(t, img1.Name, img2.Name)
	require.Equal(t, StatusReady, img2.Status)
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

	req1 := CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}
	img1, err := mgr.CreateImage(ctx, req1)
	require.NoError(t, err)

	waitForReady(t, mgr, ctx, img1.Name)

	// List should return one image
	images, err = mgr.ListImages(ctx)
	require.NoError(t, err)
	require.Len(t, images, 1)
	require.Equal(t, "docker.io/library/alpine:latest", images[0].Name)
	require.Equal(t, StatusReady, images[0].Status)
}

func TestGetImage(t *testing.T) {
	ociClient, err := NewOCIClient(t.TempDir())
	require.NoError(t, err)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, ociClient, 1)

	ctx := context.Background()
	req := CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	created, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)

	waitForReady(t, mgr, ctx, created.Name)

	img, err := mgr.GetImage(ctx, created.Name)
	require.NoError(t, err)
	require.NotNil(t, img)
	require.Equal(t, created.Name, img.Name)
	require.Equal(t, StatusReady, img.Status)
	require.NotNil(t, img.SizeBytes)
}

func TestGetImageNotFound(t *testing.T) {
	ociClient, err := NewOCIClient(t.TempDir())
	require.NoError(t, err)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, ociClient, 1)

	ctx := context.Background()

	_, err = mgr.GetImage(ctx, "nonexistent:latest")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestDeleteImage(t *testing.T) {
	ociClient, err := NewOCIClient(t.TempDir())
	require.NoError(t, err)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, ociClient, 1)

	ctx := context.Background()
	req := CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	created, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)

	waitForReady(t, mgr, ctx, created.Name)

	err = mgr.DeleteImage(ctx, created.Name)
	require.NoError(t, err)

	_, err = mgr.GetImage(ctx, created.Name)
	require.ErrorIs(t, err, ErrNotFound)

	imageDir := filepath.Join(dataDir, "images", imageNameToPath(created.Name))
	_, err = os.Stat(imageDir)
	require.True(t, os.IsNotExist(err))
}

func TestDeleteImageNotFound(t *testing.T) {
	ociClient, err := NewOCIClient(t.TempDir())
	require.NoError(t, err)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, ociClient, 1)

	ctx := context.Background()

	err = mgr.DeleteImage(ctx, "nonexistent:latest")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestImageNameToPath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"docker.io/library/nginx:latest", "docker.io/library/nginx/latest"},
		{"docker.io/library/alpine:3.18", "docker.io/library/alpine/3.18"},
		{"gcr.io/my-project/my-app:v1.0.0", "gcr.io/my-project/my-app/v1.0.0"},
		{"nginx", "nginx/latest"},
		{"ubuntu:22.04", "ubuntu/22.04"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := imageNameToPath(tt.input)
			require.Equal(t, tt.expected, result)
		})
	}
}

// waitForReady waits for an image build to complete
func waitForReady(t *testing.T, mgr Manager, ctx context.Context, imageName string) {
	for i := 0; i < 600; i++ {
		img, err := mgr.GetImage(ctx, imageName)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		if i%10 == 0 {
			t.Logf("Status: %s", img.Status)
		}

		if img.Status == StatusReady {
			return
		}

		if img.Status == StatusFailed {
			errMsg := ""
			if img.Error != nil {
				errMsg = *img.Error
			}
			t.Fatalf("Build failed: %s", errMsg)
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatal("Build did not complete within 60 seconds")
}


