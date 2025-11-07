package images

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/onkernel/hypeman/lib/oapi"
)

// Manager handles image lifecycle operations
type Manager interface {
	ListImages(ctx context.Context) ([]oapi.Image, error)
	CreateImage(ctx context.Context, req oapi.CreateImageRequest) (*oapi.Image, error)
	GetImage(ctx context.Context, id string) (*oapi.Image, error)
	DeleteImage(ctx context.Context, id string) error
}

type manager struct {
	dataDir   string
	ociClient *OCIClient
}

// NewManager creates a new image manager with OCI client
func NewManager(dataDir string, ociClient *OCIClient) Manager {
	return &manager{
		dataDir:   dataDir,
		ociClient: ociClient,
	}
}

func (m *manager) ListImages(ctx context.Context) ([]oapi.Image, error) {
	metas, err := listMetadata(m.dataDir)
	if err != nil {
		return nil, fmt.Errorf("list metadata: %w", err)
	}

	images := make([]oapi.Image, 0, len(metas))
	for _, meta := range metas {
		images = append(images, *meta.toOAPI())
	}

	return images, nil
}

func (m *manager) CreateImage(ctx context.Context, req oapi.CreateImageRequest) (*oapi.Image, error) {
	// 1. Generate or validate ID
	imageID := req.Id
	if imageID == nil || *imageID == "" {
		generated := generateImageID(req.Name)
		imageID = &generated
	}

	// 2. Check if image already exists
	if imageExists(m.dataDir, *imageID) {
		return nil, ErrAlreadyExists
	}

	// 3. Pull image and export rootfs to temp directory
	tempDir := filepath.Join(os.TempDir(), fmt.Sprintf("hypeman-image-%s-%d", *imageID, time.Now().Unix()))
	defer os.RemoveAll(tempDir) // cleanup temp dir

	containerMeta, err := m.ociClient.pullAndExport(ctx, req.Name, tempDir)
	if err != nil {
		return nil, fmt.Errorf("pull and export: %w", err)
	}

	// 5. Convert rootfs directory to ext4 disk image
	diskPath := imagePath(m.dataDir, *imageID)
	diskSize, err := convertToExt4(tempDir, diskPath)
	if err != nil {
		return nil, fmt.Errorf("convert to ext4: %w", err)
	}

	// 6. Create metadata
	meta := &imageMetadata{
		ID:         *imageID,
		Name:       req.Name,
		SizeBytes:  diskSize,
		Entrypoint: containerMeta.Entrypoint,
		Cmd:        containerMeta.Cmd,
		Env:        containerMeta.Env,
		WorkingDir: containerMeta.WorkingDir,
		CreatedAt:  time.Now(),
	}

	// 7. Write metadata atomically
	if err := writeMetadata(m.dataDir, *imageID, meta); err != nil {
		// Clean up disk image if metadata write fails
		os.Remove(diskPath)
		return nil, fmt.Errorf("write metadata: %w", err)
	}

	return meta.toOAPI(), nil
}

func (m *manager) GetImage(ctx context.Context, id string) (*oapi.Image, error) {
	meta, err := readMetadata(m.dataDir, id)
	if err != nil {
		return nil, err
	}
	return meta.toOAPI(), nil
}

func (m *manager) DeleteImage(ctx context.Context, id string) error {
	return deleteImage(m.dataDir, id)
}

// generateImageID creates a valid ID from an image name
// Example: docker.io/library/nginx:latest -> img-nginx-latest
func generateImageID(imageName string) string {
	// Extract image name and tag
	parts := strings.Split(imageName, "/")
	nameTag := parts[len(parts)-1]

	// Replace special characters with dashes
	reg := regexp.MustCompile(`[^a-zA-Z0-9]+`)
	sanitized := reg.ReplaceAllString(nameTag, "-")
	sanitized = strings.Trim(sanitized, "-")

	// Add prefix
	return "img-" + sanitized
}


