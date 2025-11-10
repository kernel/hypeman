package images

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCreateImage(t *testing.T) {
	dataDir := t.TempDir()
	mgr, err := NewManager(dataDir, 1)
	require.NoError(t, err)

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
	require.NotEmpty(t, img.Digest)
	require.Contains(t, img.Digest, "sha256:")

	// Check that digest directory exists
	ref, err := ParseNormalizedRef(img.Name)
	require.NoError(t, err)
	digestHex := strings.SplitN(img.Digest, ":", 2)[1]
	diskPath := digestPath(dataDir, ref.Repository(), digestHex)
	_, err = os.Stat(diskPath)
	require.NoError(t, err)

	// Check that tag symlink exists
	linkPath := tagSymlinkPath(dataDir, ref.Repository(), ref.Tag())
	_, err = os.Lstat(linkPath)
	require.NoError(t, err)
}

func TestCreateImageDifferentTag(t *testing.T) {
	dataDir := t.TempDir()
	mgr, err := NewManager(dataDir, 1)
	require.NoError(t, err)

	ctx := context.Background()
	req := CreateImageRequest{
		Name: "docker.io/library/alpine:3.18",
	}

	img, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, img)
	require.Equal(t, "docker.io/library/alpine:3.18", img.Name)

	waitForReady(t, mgr, ctx, img.Name)
	
	img, err = mgr.GetImage(ctx, img.Name)
	require.NoError(t, err)
	require.NotEmpty(t, img.Digest)
}

func TestCreateImageDuplicate(t *testing.T) {
	dataDir := t.TempDir()
	mgr, err := NewManager(dataDir, 1)
	require.NoError(t, err)

	ctx := context.Background()
	req := CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	img1, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)

	waitForReady(t, mgr, ctx, img1.Name)

	// Re-fetch img1 to get the complete metadata including digest
	img1, err = mgr.GetImage(ctx, img1.Name)
	require.NoError(t, err)
	require.NotEmpty(t, img1.Digest)

	// Second create should be idempotent and return existing image
	img2, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, img2)
	require.Equal(t, img1.Name, img2.Name)
	require.Equal(t, StatusReady, img2.Status)
	require.Equal(t, img1.Digest, img2.Digest) // Same digest
}

func TestListImages(t *testing.T) {
	dataDir := t.TempDir()
	mgr, err := NewManager(dataDir, 1)
	require.NoError(t, err)

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
	require.NotEmpty(t, images[0].Digest)
}

func TestGetImage(t *testing.T) {
	dataDir := t.TempDir()
	mgr, err := NewManager(dataDir, 1)
	require.NoError(t, err)

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
	require.NotEmpty(t, img.Digest)
}

func TestGetImageNotFound(t *testing.T) {
	dataDir := t.TempDir()
	mgr, err := NewManager(dataDir, 1)
	require.NoError(t, err)

	ctx := context.Background()

	_, err = mgr.GetImage(ctx, "nonexistent:latest")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestDeleteImage(t *testing.T) {
	dataDir := t.TempDir()
	mgr, err := NewManager(dataDir, 1)
	require.NoError(t, err)

	ctx := context.Background()
	req := CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	created, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)

	waitForReady(t, mgr, ctx, created.Name)

	// Get the digest before deleting
	img, err := mgr.GetImage(ctx, created.Name)
	require.NoError(t, err)
	ref, err := ParseNormalizedRef(img.Name)
	require.NoError(t, err)
	digestHex := strings.SplitN(img.Digest, ":", 2)[1]

	err = mgr.DeleteImage(ctx, created.Name)
	require.NoError(t, err)

	// Tag should be gone
	_, err = mgr.GetImage(ctx, created.Name)
	require.ErrorIs(t, err, ErrNotFound)

	// But digest directory should still exist
	digestDir := digestPath(dataDir, ref.Repository(), digestHex)
	_, err = os.Stat(digestDir)
	require.NoError(t, err)
}

func TestDeleteImageNotFound(t *testing.T) {
	dataDir := t.TempDir()
	mgr, err := NewManager(dataDir, 1)
	require.NoError(t, err)

	ctx := context.Background()

	err = mgr.DeleteImage(ctx, "nonexistent:latest")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestNormalizedRefParsing(t *testing.T) {
	tests := []struct {
		input      string
		expectRepo string
		expectTag  string
	}{
		{"alpine", "docker.io/library/alpine", "latest"},
		{"alpine:3.18", "docker.io/library/alpine", "3.18"},
		{"docker.io/library/alpine:latest", "docker.io/library/alpine", "latest"},
		{"ghcr.io/myorg/myapp:v1.0.0", "ghcr.io/myorg/myapp", "v1.0.0"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			ref, err := ParseNormalizedRef(tt.input)
			require.NoError(t, err)

			repo := ref.Repository()
			require.Equal(t, tt.expectRepo, repo)

			tag := ref.Tag()
			require.Equal(t, tt.expectTag, tag)
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
