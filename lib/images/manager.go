package images

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/onkernel/hypeman/lib/paths"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
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

// Metrics holds the metrics instruments for image operations.
type Metrics struct {
	buildDuration metric.Float64Histogram
	pullsTotal    metric.Int64Counter
}

type manager struct {
	paths     *paths.Paths
	ociClient *ociClient
	queue     *BuildQueue
	createMu  sync.Mutex
	metrics   *Metrics
}

// NewManager creates a new image manager.
// If meter is nil, metrics are disabled.
func NewManager(p *paths.Paths, maxConcurrentBuilds int, meter metric.Meter) (Manager, error) {
	// Create cache directory under dataDir for OCI layouts
	cacheDir := p.SystemOCICache()
	ociClient, err := newOCIClient(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("create oci client: %w", err)
	}

	m := &manager{
		paths:     p,
		ociClient: ociClient,
		queue:     NewBuildQueue(maxConcurrentBuilds),
	}

	// Initialize metrics if meter is provided
	if meter != nil {
		metrics, err := newMetrics(meter, m)
		if err != nil {
			return nil, fmt.Errorf("create metrics: %w", err)
		}
		m.metrics = metrics
	}

	m.RecoverInterruptedBuilds()
	return m, nil
}

// newMetrics creates and registers all image metrics.
func newMetrics(meter metric.Meter, m *manager) (*Metrics, error) {
	buildDuration, err := meter.Float64Histogram(
		"hypeman_images_build_duration_seconds",
		metric.WithDescription("Time to build an image"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	pullsTotal, err := meter.Int64Counter(
		"hypeman_images_pulls_total",
		metric.WithDescription("Total number of image pulls from registries"),
	)
	if err != nil {
		return nil, err
	}

	// Register observable gauges for queue length and total images
	buildQueueLength, err := meter.Int64ObservableGauge(
		"hypeman_images_build_queue_length",
		metric.WithDescription("Current number of images in the build queue"),
	)
	if err != nil {
		return nil, err
	}

	imagesTotal, err := meter.Int64ObservableGauge(
		"hypeman_images_total",
		metric.WithDescription("Total number of cached images"),
	)
	if err != nil {
		return nil, err
	}

	_, err = meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			// Report queue length
			o.ObserveInt64(buildQueueLength, int64(m.queue.QueueLength()))

			// Count images by status
			metas, err := listAllTags(m.paths)
			if err != nil {
				return nil
			}
			statusCounts := make(map[string]int64)
			for _, meta := range metas {
				statusCounts[meta.Status]++
			}
			for status, count := range statusCounts {
				o.ObserveInt64(imagesTotal, count,
					metric.WithAttributes(attribute.String("status", status)))
			}
			return nil
		},
		buildQueueLength,
		imagesTotal,
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		buildDuration: buildDuration,
		pullsTotal:    pullsTotal,
	}, nil
}

func (m *manager) ListImages(ctx context.Context) ([]Image, error) {
	metas, err := listAllTags(m.paths)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}

	images := make([]Image, 0, len(metas))
	for _, meta := range metas {
		images = append(images, *meta.toImage())
	}

	return images, nil
}

func (m *manager) CreateImage(ctx context.Context, req CreateImageRequest) (*Image, error) {
	// Parse and normalize
	normalized, err := ParseNormalizedRef(req.Name)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidName, err.Error())
	}

	// Resolve to get digest (validates existence)
	// Add a 2-second timeout to ensure fast failure on rate limits or errors
	resolveCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	ref, err := normalized.Resolve(resolveCtx, m.ociClient)
	if err != nil {
		return nil, fmt.Errorf("resolve manifest: %w", err)
	}

	m.createMu.Lock()
	defer m.createMu.Unlock()

	// Check if we already have this digest (deduplication)
	if meta, err := readMetadata(m.paths, ref.Repository(), ref.DigestHex()); err == nil {
		// We have this digest already
		if meta.Status == StatusReady && ref.Tag() != "" {
			// Update tag symlink to point to current digest
			// (handles case where tag moved to new digest)
			createTagSymlink(m.paths, ref.Repository(), ref.Tag(), ref.DigestHex())
		}
		img := meta.toImage()
		// Add queue position if pending
		if meta.Status == StatusPending {
			img.QueuePosition = m.queue.GetPosition(meta.Digest)
		}
		return img, nil
	}

	// Don't have this digest yet, queue the build
	return m.createAndQueueImage(ref)
}

func (m *manager) createAndQueueImage(ref *ResolvedRef) (*Image, error) {
	meta := &imageMetadata{
		Name:      ref.String(),
		Digest:    ref.Digest(),
		Status:    StatusPending,
		Request:   &CreateImageRequest{Name: ref.String()},
		CreatedAt: time.Now(),
	}

	// Write initial metadata
	if err := writeMetadata(m.paths, ref.Repository(), ref.DigestHex(), meta); err != nil {
		return nil, fmt.Errorf("write initial metadata: %w", err)
	}

	// Enqueue the build using digest as the queue key for deduplication
	queuePos := m.queue.Enqueue(ref.Digest(), CreateImageRequest{Name: ref.String()}, func() {
		m.buildImage(context.Background(), ref)
	})

	img := meta.toImage()
	if queuePos > 0 {
		img.QueuePosition = &queuePos
	}
	return img, nil
}

func (m *manager) buildImage(ctx context.Context, ref *ResolvedRef) {
	buildStart := time.Now()
	buildDir := m.paths.SystemBuild(ref.String())
	tempDir := filepath.Join(buildDir, "rootfs")

	if err := os.MkdirAll(buildDir, 0755); err != nil {
		m.updateStatusByDigest(ref, StatusFailed, fmt.Errorf("create build dir: %w", err))
		m.recordBuildMetrics(ctx, buildStart, "failed")
		return
	}

	defer func() {
		// Clean up build directory after completion
		os.RemoveAll(buildDir)
	}()

	m.updateStatusByDigest(ref, StatusPulling, nil)

	// Pull the image (digest is always known, uses cache if already pulled)
	result, err := m.ociClient.pullAndExport(ctx, ref.String(), ref.Digest(), tempDir)
	if err != nil {
		m.updateStatusByDigest(ref, StatusFailed, fmt.Errorf("pull and export: %w", err))
		m.recordPullMetrics(ctx, "failed")
		m.recordBuildMetrics(ctx, buildStart, "failed")
		return
	}
	m.recordPullMetrics(ctx, "success")

	// Check if this digest already exists and is ready (deduplication)
	if meta, err := readMetadata(m.paths, ref.Repository(), ref.DigestHex()); err == nil {
		if meta.Status == StatusReady {
			// Another build completed first, just update the tag symlink
			if ref.Tag() != "" {
				createTagSymlink(m.paths, ref.Repository(), ref.Tag(), ref.DigestHex())
			}
			return
		}
	}

	m.updateStatusByDigest(ref, StatusConverting, nil)

	diskPath := digestPath(m.paths, ref.Repository(), ref.DigestHex())
	// Use default image format (ext4 for now, easy to switch to erofs later)
	diskSize, err := ExportRootfs(tempDir, diskPath, DefaultImageFormat)
	if err != nil {
		m.updateStatusByDigest(ref, StatusFailed, fmt.Errorf("convert to %s: %w", DefaultImageFormat, err))
		return
	}

	// Read current metadata to preserve request info
	meta, err := readMetadata(m.paths, ref.Repository(), ref.DigestHex())
	if err != nil {
		// Create new metadata if it doesn't exist
		meta = &imageMetadata{
			Name:      ref.String(),
			Digest:    ref.Digest(),
			CreatedAt: time.Now(),
		}
	}

	// Update with final status
	meta.Status = StatusReady
	meta.Error = nil
	meta.SizeBytes = diskSize
	meta.Entrypoint = result.Metadata.Entrypoint
	meta.Cmd = result.Metadata.Cmd
	meta.Env = result.Metadata.Env
	meta.WorkingDir = result.Metadata.WorkingDir

	if err := writeMetadata(m.paths, ref.Repository(), ref.DigestHex(), meta); err != nil {
		m.updateStatusByDigest(ref, StatusFailed, fmt.Errorf("write final metadata: %w", err))
		return
	}

	// Only create/update tag symlink on successful completion
	if ref.Tag() != "" {
		if err := createTagSymlink(m.paths, ref.Repository(), ref.Tag(), ref.DigestHex()); err != nil {
			// Log error but don't fail the build
			fmt.Fprintf(os.Stderr, "Warning: failed to create tag symlink: %v\n", err)
		}
	}

	m.recordBuildMetrics(ctx, buildStart, "success")
}

// recordBuildMetrics records the build duration metric.
func (m *manager) recordBuildMetrics(ctx context.Context, start time.Time, status string) {
	if m.metrics == nil {
		return
	}
	duration := time.Since(start).Seconds()
	m.metrics.buildDuration.Record(ctx, duration,
		metric.WithAttributes(attribute.String("status", status)))
}

// recordPullMetrics records the pull counter metric.
func (m *manager) recordPullMetrics(ctx context.Context, status string) {
	if m.metrics == nil {
		return
	}
	m.metrics.pullsTotal.Add(ctx, 1,
		metric.WithAttributes(attribute.String("status", status)))
}

func (m *manager) updateStatusByDigest(ref *ResolvedRef, status string, err error) {
	meta, readErr := readMetadata(m.paths, ref.Repository(), ref.DigestHex())
	if readErr != nil {
		// Create new metadata if it doesn't exist
		meta = &imageMetadata{
			Name:      ref.String(),
			Digest:    ref.Digest(),
			Status:    status,
			CreatedAt: time.Now(),
		}
	} else {
		meta.Status = status
	}

	if err != nil {
		errorMsg := err.Error()
		meta.Error = &errorMsg
	}

	writeMetadata(m.paths, ref.Repository(), ref.DigestHex(), meta)
}

func (m *manager) RecoverInterruptedBuilds() {
	metas, err := listAllTags(m.paths)
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
			if meta.Request != nil && meta.Digest != "" {
				metaCopy := meta
				normalized, err := ParseNormalizedRef(metaCopy.Name)
				if err != nil {
					continue
				}
				// Create a ResolvedRef since we already have the digest from metadata
				ref := NewResolvedRef(normalized, metaCopy.Digest)
				m.queue.Enqueue(metaCopy.Digest, *metaCopy.Request, func() {
					m.buildImage(context.Background(), ref)
				})
			}
		}
	}
}

func (m *manager) GetImage(ctx context.Context, name string) (*Image, error) {
	// Parse and normalize the reference
	ref, err := ParseNormalizedRef(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidName, err.Error())
	}

	repository := ref.Repository()

	var digestHex string

	if ref.IsDigest() {
		// Direct digest lookup
		digestHex = ref.DigestHex()
	} else {
		// Tag lookup - resolve symlink
		tag := ref.Tag()

		digestHex, err = resolveTag(m.paths, repository, tag)
		if err != nil {
			return nil, err
		}
	}

	meta, err := readMetadata(m.paths, repository, digestHex)
	if err != nil {
		return nil, err
	}

	img := meta.toImage()

	if meta.Status == StatusPending {
		img.QueuePosition = m.queue.GetPosition(meta.Digest)
	}

	return img, nil
}

func (m *manager) DeleteImage(ctx context.Context, name string) error {
	// Parse and normalize the reference
	ref, err := ParseNormalizedRef(name)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidName, err.Error())
	}

	// Only allow deleting by tag, not by digest
	if ref.IsDigest() {
		return fmt.Errorf("cannot delete by digest, use tag name instead")
	}

	repository := ref.Repository()
	tag := ref.Tag()

	return deleteTag(m.paths, repository, tag)
}
