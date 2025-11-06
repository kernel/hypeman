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
	dockerClient := skipIfNoDocker(t)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, dockerClient)

	ctx := context.Background()
	req := oapi.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	img, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, img)
	require.Equal(t, "docker.io/library/alpine:latest", img.Name)
	require.Equal(t, "img-alpine-latest", img.Id)
	require.NotNil(t, img.SizeBytes)
	require.Greater(t, *img.SizeBytes, int64(0))

	// Verify disk image was created
	diskPath := filepath.Join(dataDir, "images", img.Id, "rootfs.ext4")
	_, err = os.Stat(diskPath)
	require.NoError(t, err)

	// Verify metadata file was created
	metaPath := filepath.Join(dataDir, "images", img.Id, "metadata.json")
	_, err = os.Stat(metaPath)
	require.NoError(t, err)
}

func TestCreateImageWithCustomID(t *testing.T) {
	dockerClient := skipIfNoDocker(t)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, dockerClient)

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
}

func TestCreateImageDuplicate(t *testing.T) {
	dockerClient := skipIfNoDocker(t)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, dockerClient)

	ctx := context.Background()
	req := oapi.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	// Create first image
	_, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)

	// Try to create duplicate
	_, err = mgr.CreateImage(ctx, req)
	require.ErrorIs(t, err, ErrAlreadyExists)
}

func TestListImages(t *testing.T) {
	dockerClient := skipIfNoDocker(t)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, dockerClient)

	ctx := context.Background()

	// Initially empty
	images, err := mgr.ListImages(ctx)
	require.NoError(t, err)
	require.Len(t, images, 0)

	// Create first image
	req1 := oapi.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}
	_, err = mgr.CreateImage(ctx, req1)
	require.NoError(t, err)

	// List should return one image
	images, err = mgr.ListImages(ctx)
	require.NoError(t, err)
	require.Len(t, images, 1)
	require.Equal(t, "docker.io/library/alpine:latest", images[0].Name)
}

func TestGetImage(t *testing.T) {
	dockerClient := skipIfNoDocker(t)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, dockerClient)

	ctx := context.Background()
	req := oapi.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	created, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)

	// Get the image
	img, err := mgr.GetImage(ctx, created.Id)
	require.NoError(t, err)
	require.NotNil(t, img)
	require.Equal(t, created.Id, img.Id)
	require.Equal(t, created.Name, img.Name)
	require.Equal(t, *created.SizeBytes, *img.SizeBytes)
}

func TestGetImageNotFound(t *testing.T) {
	dockerClient, _ := NewDockerClient()
	if dockerClient != nil {
		defer dockerClient.Close()
	}

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, dockerClient)

	ctx := context.Background()

	// Try to get non-existent image
	_, err := mgr.GetImage(ctx, "nonexistent")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestDeleteImage(t *testing.T) {
	dockerClient := skipIfNoDocker(t)

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, dockerClient)

	ctx := context.Background()
	req := oapi.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	created, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)

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
	dockerClient, _ := NewDockerClient()
	if dockerClient != nil {
		defer dockerClient.Close()
	}

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, dockerClient)

	ctx := context.Background()

	// Try to delete non-existent image
	err := mgr.DeleteImage(ctx, "nonexistent")
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

// skipIfNoDocker skips the test if Docker is not available or accessible
// Returns a DockerClient for use in tests
func skipIfNoDocker(t *testing.T) *DockerClient {
	// Try to connect to Docker to verify we have permission
	client, err := NewDockerClient()
	if err != nil {
		t.Skipf("cannot connect to docker: %v, skipping test", err)
	}

	// Verify we can actually use Docker by pinging it
	ctx := context.Background()
	_, err = client.cli.Ping(ctx)
	if err != nil {
		client.Close()
		t.Skipf("docker not available: %v, skipping test", err)
	}

	return client
}

