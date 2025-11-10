package images

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/distribution/reference"
	"github.com/onkernel/hypeman/lib/oapi"
)

const (
	StatusPending    = "pending"
	StatusPulling    = "pulling"
	StatusConverting = "converting"
	StatusReady      = "ready"
	StatusFailed     = "failed"
)

// Manager handles image lifecycle operations
type Manager interface {
	ListImages(ctx context.Context) ([]oapi.Image, error)
	CreateImage(ctx context.Context, req oapi.CreateImageRequest) (*oapi.Image, error)
	GetImage(ctx context.Context, id string) (*oapi.Image, error)
	DeleteImage(ctx context.Context, id string) error
	RecoverInterruptedBuilds()
}

type manager struct {
	dataDir   string
	ociClient *OCIClient
	queue     *BuildQueue
}

// NewManager creates a new image manager with OCI client
func NewManager(dataDir string, ociClient *OCIClient, maxConcurrentBuilds int) Manager {
	m := &manager{
		dataDir:   dataDir,
		ociClient: ociClient,
		queue:     NewBuildQueue(maxConcurrentBuilds),
	}
	m.RecoverInterruptedBuilds()
	return m
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
	normalizedName, err := normalizeImageName(req.Name)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidName, err.Error())
	}

	if imageExists(m.dataDir, normalizedName) {
		return nil, ErrAlreadyExists
	}

	meta := &imageMetadata{
		Name:      normalizedName,
		Status:    StatusPending,
		Request:   &req,
		CreatedAt: time.Now(),
	}

	if err := writeMetadata(m.dataDir, normalizedName, meta); err != nil {
		return nil, fmt.Errorf("write initial metadata: %w", err)
	}

	queuePos := m.queue.Enqueue(normalizedName, req, func() {
		m.buildImage(context.Background(), normalizedName, req)
	})

	img := meta.toOAPI()
	if queuePos > 0 {
		img.QueuePosition = &queuePos
	}
	return img, nil
}

func (m *manager) buildImage(ctx context.Context, imageName string, req oapi.CreateImageRequest) {
	defer m.queue.MarkComplete(imageName)

	buildDir := filepath.Join(imageDir(m.dataDir, imageName), ".build")
	tempDir := filepath.Join(buildDir, "rootfs")

	if err := os.MkdirAll(buildDir, 0755); err != nil {
		m.updateStatus(imageName, StatusFailed, fmt.Errorf("create build dir: %w", err))
		return
	}

	defer func() {
		meta, _ := readMetadata(m.dataDir, imageName)
		if meta != nil && meta.Status == StatusReady {
			os.RemoveAll(buildDir)
		}
	}()

	m.updateStatus(imageName, StatusPulling, nil)
	containerMeta, err := m.ociClient.pullAndExport(ctx, req.Name, tempDir)
	if err != nil {
		m.updateStatus(imageName, StatusFailed, fmt.Errorf("pull and export: %w", err))
		return
	}

	m.updateStatus(imageName, StatusConverting, nil)
	diskPath := imagePath(m.dataDir, imageName)
	diskSize, err := convertToExt4(tempDir, diskPath)
	if err != nil {
		m.updateStatus(imageName, StatusFailed, fmt.Errorf("convert to ext4: %w", err))
		return
	}

	meta, err := readMetadata(m.dataDir, imageName)
	if err != nil {
		m.updateStatus(imageName, StatusFailed, fmt.Errorf("read metadata: %w", err))
		return
	}

	meta.Status = StatusReady
	meta.Error = nil
	meta.SizeBytes = diskSize
	meta.Entrypoint = containerMeta.Entrypoint
	meta.Cmd = containerMeta.Cmd
	meta.Env = containerMeta.Env
	meta.WorkingDir = containerMeta.WorkingDir

	if err := writeMetadata(m.dataDir, imageName, meta); err != nil {
		m.updateStatus(imageName, StatusFailed, fmt.Errorf("write final metadata: %w", err))
		return
	}
}

func (m *manager) updateStatus(imageName, status string, err error) {
	meta, readErr := readMetadata(m.dataDir, imageName)
	if readErr != nil {
		return
	}

	meta.Status = status
	if err != nil {
		errorMsg := err.Error()
		meta.Error = &errorMsg
	}

	writeMetadata(m.dataDir, imageName, meta)
}

func (m *manager) RecoverInterruptedBuilds() {
	metas, err := listMetadata(m.dataDir)
	if err != nil {
		return // Best effort
	}

	// Sort by created_at to maintain FIFO order
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].CreatedAt.Before(metas[j].CreatedAt)
	})

	for _, meta := range metas {
		switch meta.Status {
		case StatusPending, StatusPulling, StatusConverting:
			if meta.Request != nil {
				metaCopy := meta
				m.queue.Enqueue(metaCopy.Name, *metaCopy.Request, func() {
					m.buildImage(context.Background(), metaCopy.Name, *metaCopy.Request)
				})
			}
		}
	}
}

func (m *manager) GetImage(ctx context.Context, name string) (*oapi.Image, error) {
	normalizedName, err := normalizeImageName(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidName, err.Error())
	}

	meta, err := readMetadata(m.dataDir, normalizedName)
	if err != nil {
		return nil, err
	}
	
	img := meta.toOAPI()
	
	if meta.Status == StatusPending {
		img.QueuePosition = m.queue.GetPosition(normalizedName)
	}
	
	return img, nil
}

func (m *manager) DeleteImage(ctx context.Context, name string) error {
	normalizedName, err := normalizeImageName(name)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidName, err.Error())
	}
	return deleteImage(m.dataDir, normalizedName)
}

// normalizeImageName validates and normalizes an OCI image reference
// Examples: alpine → docker.io/library/alpine:latest
//           nginx:1.0 → docker.io/library/nginx:1.0
func normalizeImageName(name string) (string, error) {
	named, err := reference.ParseNormalizedNamed(name)
	if err != nil {
		return "", err
	}
	
	// Ensure it has a tag (add :latest if missing)
	tagged := reference.TagNameOnly(named)
	return tagged.String(), nil
}


