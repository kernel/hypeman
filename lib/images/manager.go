package images

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/distribution/reference"
)

const (
	StatusPending    = "pending"
	StatusPulling    = "pulling"
	StatusConverting = "converting"
	StatusReady      = "ready"
	StatusFailed     = "failed"
)

type Manager interface {
	ListImages(ctx context.Context) ([]Image, error)
	CreateImage(ctx context.Context, req CreateImageRequest) (*Image, error)
	GetImage(ctx context.Context, name string) (*Image, error)
	DeleteImage(ctx context.Context, name string) error
	RecoverInterruptedBuilds()
}

type manager struct {
	dataDir  string
	ociClient *OCIClient
	queue     *BuildQueue
	createMu  sync.Mutex
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

func (m *manager) ListImages(ctx context.Context) ([]Image, error) {
	metas, err := listMetadata(m.dataDir)
	if err != nil {
		return nil, fmt.Errorf("list metadata: %w", err)
	}

	images := make([]Image, 0, len(metas))
	for _, meta := range metas {
		images = append(images, *meta.toImage())
	}

	return images, nil
}

func (m *manager) CreateImage(ctx context.Context, req CreateImageRequest) (*Image, error) {
	normalizedName, err := normalizeImageName(req.Name)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidName, err.Error())
	}

	m.createMu.Lock()
	defer m.createMu.Unlock()

	// Check if already exists (handles ready, failed, and in-progress)
	if meta, err := readMetadata(m.dataDir, normalizedName); err == nil {
		img := meta.toImage()
		// Add dynamic queue position only for pending status
		if meta.Status == StatusPending {
			img.QueuePosition = m.queue.GetPosition(normalizedName)
		}
		return img, nil
	}

	// Create metadata (we know it doesn't exist)
	meta := &imageMetadata{
		Name:      normalizedName,
		Status:    StatusPending,
		Request:   &req,
		CreatedAt: time.Now(),
	}

	if err := writeMetadata(m.dataDir, normalizedName, meta); err != nil {
		return nil, fmt.Errorf("write initial metadata: %w", err)
	}

	// Enqueue (we know it's not already queued due to mutex)
	queuePos := m.queue.Enqueue(normalizedName, req, func() {
		m.buildImage(context.Background(), normalizedName, req)
	})

	img := meta.toImage()
	if queuePos > 0 {
		img.QueuePosition = &queuePos
	}
	return img, nil
}

func (m *manager) buildImage(ctx context.Context, imageName string, req CreateImageRequest) {

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
	diskSize, err := convertToErofs(tempDir, diskPath)
	if err != nil {
		m.updateStatus(imageName, StatusFailed, fmt.Errorf("convert to erofs: %w", err))
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

func (m *manager) GetImage(ctx context.Context, name string) (*Image, error) {
	normalizedName, err := normalizeImageName(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidName, err.Error())
	}

	meta, err := readMetadata(m.dataDir, normalizedName)
	if err != nil {
		return nil, err
	}
	
	img := meta.toImage()
	
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


