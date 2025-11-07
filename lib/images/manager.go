package images

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/onkernel/hypeman/lib/oapi"
)

// Manager handles image lifecycle operations
type Manager interface {
	ListImages(ctx context.Context) ([]oapi.Image, error)
	CreateImage(ctx context.Context, req oapi.CreateImageRequest) (*oapi.Image, error)
	GetImage(ctx context.Context, id string) (*oapi.Image, error)
	DeleteImage(ctx context.Context, id string) error
	GetProgress(ctx context.Context, id string) (chan ProgressUpdate, error)
	RecoverInterruptedBuilds()
}

type manager struct {
	dataDir   string
	ociClient *OCIClient
	queue     *BuildQueue
	trackers  map[string]*ProgressTracker
	mu        sync.RWMutex
}

// NewManager creates a new image manager with OCI client
func NewManager(dataDir string, ociClient *OCIClient, maxConcurrentBuilds int) Manager {
	m := &manager{
		dataDir:   dataDir,
		ociClient: ociClient,
		queue:     NewBuildQueue(maxConcurrentBuilds),
		trackers:  make(map[string]*ProgressTracker),
	}
	// Recover interrupted builds on initialization
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

	// 3. Create initial metadata with pending status
	meta := &imageMetadata{
		ID:        *imageID,
		Name:      req.Name,
		Status:    StatusPending,
		Progress:  0,
		Request:   &req,
		CreatedAt: time.Now(),
	}

	if err := writeMetadata(m.dataDir, *imageID, meta); err != nil {
		return nil, fmt.Errorf("write initial metadata: %w", err)
	}

	// 4. Enqueue the build
	queuePos := m.queue.Enqueue(*imageID, req, func() {
		m.buildImage(context.Background(), *imageID, req)
	})

	meta.QueuePosition = &queuePos
	if err := writeMetadata(m.dataDir, *imageID, meta); err != nil {
		return nil, fmt.Errorf("update queue position: %w", err)
	}

	// 5. Return immediately (build happens in background)
	return meta.toOAPI(), nil
}

// buildImage performs the actual image build in the background
func (m *manager) buildImage(ctx context.Context, imageID string, req oapi.CreateImageRequest) {
	// Create progress tracker
	tracker := NewProgressTracker(imageID, m.dataDir)
	m.registerTracker(imageID, tracker)
	defer tracker.Close()
	defer m.unregisterTracker(imageID)
	defer m.queue.MarkComplete(imageID)

	// Use persistent build directory for resumability
	buildDir := filepath.Join(imageDir(m.dataDir, imageID), ".build")
	tempDir := filepath.Join(buildDir, "rootfs")

	// Ensure build directory exists
	if err := os.MkdirAll(buildDir, 0755); err != nil {
		tracker.Fail(fmt.Errorf("create build dir: %w", err))
		return
	}

	// Cleanup build dir on success
	defer func() {
		meta, _ := readMetadata(m.dataDir, imageID)
		if meta != nil && meta.Status == StatusReady {
			os.RemoveAll(buildDir)
		}
	}()

	// Phase 1: Pull and unpack (0-90%)
	tracker.Update(StatusPulling, 0, nil)
	containerMeta, err := m.ociClient.pullAndExportWithProgress(ctx, req.Name, tempDir, tracker)
	if err != nil {
		tracker.Fail(fmt.Errorf("pull and export: %w", err))
		return
	}

	// Phase 2: Convert to ext4 (90-100%)
	tracker.Update(StatusConverting, 90, nil)
	diskPath := imagePath(m.dataDir, imageID)
	diskSize, err := convertToExt4(tempDir, diskPath)
	if err != nil {
		tracker.Fail(fmt.Errorf("convert to ext4: %w", err))
		return
	}

	// Phase 3: Finalize metadata
	meta, err := readMetadata(m.dataDir, imageID)
	if err != nil {
		tracker.Fail(fmt.Errorf("read metadata: %w", err))
		return
	}

	meta.Status = StatusReady
	meta.Progress = 100
	meta.QueuePosition = nil
	meta.Error = nil
	meta.SizeBytes = diskSize
	meta.Entrypoint = containerMeta.Entrypoint
	meta.Cmd = containerMeta.Cmd
	meta.Env = containerMeta.Env
	meta.WorkingDir = containerMeta.WorkingDir

	if err := writeMetadata(m.dataDir, imageID, meta); err != nil {
		tracker.Fail(fmt.Errorf("write final metadata: %w", err))
		return
	}

	tracker.Complete()
}

// GetProgress returns a channel for SSE progress updates
func (m *manager) GetProgress(ctx context.Context, id string) (chan ProgressUpdate, error) {
	// Get or create tracker
	m.mu.Lock()
	tracker, exists := m.trackers[id]
	if !exists {
		// Check if image exists before creating tracker
		if !imageExists(m.dataDir, id) {
			m.mu.Unlock()
			return nil, ErrNotFound
		}
		// No active build, create temporary tracker that sends current state
		tracker = NewProgressTracker(id, m.dataDir)
		m.trackers[id] = tracker
	}
	m.mu.Unlock()

	// Subscribe to progress updates
	ch, err := tracker.Subscribe(ctx)
	if err != nil {
		return nil, fmt.Errorf("subscribe to progress: %w", err)
	}

	return ch, nil
}

// RecoverInterruptedBuilds resumes builds that were interrupted by server restart
func (m *manager) RecoverInterruptedBuilds() {
	metas, err := listMetadata(m.dataDir)
	if err != nil {
		return // Best effort
	}

	for _, meta := range metas {
		switch meta.Status {
		case StatusPending, StatusPulling, StatusUnpacking, StatusConverting:
			// Re-enqueue the build
			if meta.Request != nil {
				m.queue.Enqueue(meta.ID, *meta.Request, func() {
					m.buildImage(context.Background(), meta.ID, *meta.Request)
				})
			}
		}
	}
}

// registerTracker adds a tracker to the active map
func (m *manager) registerTracker(imageID string, tracker *ProgressTracker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.trackers[imageID] = tracker
}

// unregisterTracker removes a tracker from the active map
func (m *manager) unregisterTracker(imageID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.trackers, imageID)
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


