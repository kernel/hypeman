package images

import (
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/archive"
)

// DockerClient wraps Docker API operations
type DockerClient struct {
	cli *client.Client
}

// NewDockerClient creates a new Docker client using environment variables
func NewDockerClient() (*DockerClient, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &DockerClient{cli: cli}, nil
}

// Close closes the Docker client
func (d *DockerClient) Close() error {
	return d.cli.Close()
}

// pullAndExport pulls an OCI image and exports its rootfs to a directory
func (d *DockerClient) pullAndExport(ctx context.Context, imageRef, exportDir string) (*containerMetadata, error) {
	// Pull the image
	pullReader, err := d.cli.ImagePull(ctx, imageRef, image.PullOptions{})
	if err != nil {
		return nil, fmt.Errorf("pull image: %w", err)
	}
	// Consume the pull output (required for pull to complete)
	io.Copy(io.Discard, pullReader)
	pullReader.Close()

	// Inspect image to get metadata
	inspect, _, err := d.cli.ImageInspectWithRaw(ctx, imageRef)
	if err != nil {
		return nil, fmt.Errorf("inspect image: %w", err)
	}

	// Extract metadata from inspect response
	meta := &containerMetadata{
		Entrypoint: inspect.Config.Entrypoint,
		Cmd:        inspect.Config.Cmd,
		Env:        make(map[string]string),
		WorkingDir: inspect.Config.WorkingDir,
	}

	// Parse environment variables
	for _, env := range inspect.Config.Env {
		for i := 0; i < len(env); i++ {
			if env[i] == '=' {
				key := env[:i]
				val := env[i+1:]
				meta.Env[key] = val
				break
			}
		}
	}

	// Create a temporary container from the image
	// Creating a container does not run it,
	// it does not execute any code inside the container.
	containerResp, err := d.cli.ContainerCreate(ctx, &container.Config{
		Image: imageRef,
	}, nil, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}

	// Ensure container is removed even if export fails
	defer func() {
		d.cli.ContainerRemove(ctx, containerResp.ID, container.RemoveOptions{})
	}()

	// Export container filesystem as tar stream
	exportReader, err := d.cli.ContainerExport(ctx, containerResp.ID)
	if err != nil {
		return nil, fmt.Errorf("export container: %w", err)
	}
	defer exportReader.Close()

	// Extract tar to exportDir
	if err := extractTar(exportReader, exportDir); err != nil {
		return nil, fmt.Errorf("extract tar: %w", err)
	}

	return meta, nil
}

// containerMetadata holds extracted container metadata
type containerMetadata struct {
	Entrypoint []string
	Cmd        []string
	Env        map[string]string
	WorkingDir string
}

// extractTar extracts a tar stream to a target directory using Docker's archive package
func extractTar(reader io.Reader, targetDir string) error {
	// Use Docker's battle-tested archive extraction with security hardening
	return archive.Untar(reader, targetDir, &archive.TarOptions{
		NoLchown: true, // Don't change ownership (we're not root)
	})
}
