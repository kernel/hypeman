package images

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/kernel/hypeman/lib/paths"
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
	// ImportLocalImage imports an image that was pushed to the local OCI cache.
	// Unlike CreateImage, it does not resolve from a remote registry.
	ImportLocalImage(ctx context.Context, repo, reference, digest string) (*Image, error)
	// RegisterPrebuiltImage registers a pre-built erofs disk as a ready image.
	// This skips the slow umoci unpack + mkfs.erofs conversion pipeline by using
	// a disk that was already built inside the builder VM.
	// The diskSrcPath is a temporary file that will be moved/copied to the image store.
	// Metadata is extracted from the OCI cache (populated asynchronously by registry push).
	RegisterPrebuiltImage(ctx context.Context, repo, digest, name string, diskSrcPath string) error
	GetImage(ctx context.Context, name string) (*Image, error)
	DeleteImage(ctx context.Context, name string) error
	RecoverInterruptedBuilds()
	// TotalImageBytes returns the total size of all ready images on disk.
	// Used by the resource manager for disk capacity tracking.
	TotalImageBytes(ctx context.Context) (int64, error)
	// TotalOCICacheBytes returns the total size of the OCI layer cache.
	// Used by the resource manager for disk capacity tracking.
	TotalOCICacheBytes(ctx context.Context) (int64, error)
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
		// Don't cache failed builds - allow retry
		if meta.Status == StatusFailed {
			// Clean up the failed build directory so we can retry
			digestDir := filepath.Join(m.paths.ImagesDir(), ref.Repository(), ref.DigestHex())
			os.RemoveAll(digestDir)
			// Fall through to re-queue the build
		} else {
			// We have this digest already (ready, pending, pulling, or converting)
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
	}

	// Don't have this digest yet, queue the build
	return m.createAndQueueImage(ref)
}

// ImportLocalImage imports an image from the local OCI cache without resolving from a remote registry.
// This is used for images that were pushed directly to the hypeman registry.
func (m *manager) ImportLocalImage(ctx context.Context, repo, reference, digest string) (*Image, error) {
	// Build the image reference string
	var imageRef string
	if strings.HasPrefix(reference, "sha256:") {
		imageRef = repo + "@" + reference
	} else {
		imageRef = repo + ":" + reference
	}

	// Parse and normalize
	normalized, err := ParseNormalizedRef(imageRef)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidName, err.Error())
	}

	// Create a ResolvedRef directly with the provided digest
	ref := NewResolvedRef(normalized, digest)

	m.createMu.Lock()
	defer m.createMu.Unlock()

	// Check if we already have this digest (deduplication)
	if meta, err := readMetadata(m.paths, ref.Repository(), ref.DigestHex()); err == nil {
		// Don't cache failed builds - allow retry
		if meta.Status == StatusFailed {
			digestDir := filepath.Join(m.paths.ImagesDir(), ref.Repository(), ref.DigestHex())
			os.RemoveAll(digestDir)
			// Fall through to re-queue the build
		} else {
			// We have this digest already
			if meta.Status == StatusReady && ref.Tag() != "" {
				createTagSymlink(m.paths, ref.Repository(), ref.Tag(), ref.DigestHex())
			}
			img := meta.toImage()
			if meta.Status == StatusPending {
				img.QueuePosition = m.queue.GetPosition(meta.Digest)
			}
			return img, nil
		}
	}

	// Don't have this digest yet, queue the build
	return m.createAndQueueImage(ref)
}

// RegisterPrebuiltImage registers a pre-built erofs disk as a ready image,
// skipping the slow umoci unpack + mkfs conversion pipeline.
func (m *manager) RegisterPrebuiltImage(ctx context.Context, repo, digestStr, imageName string, diskSrcPath string) error {
	digestHex := strings.TrimPrefix(digestStr, "sha256:")

	m.createMu.Lock()
	defer m.createMu.Unlock()

	// Check if this digest already exists and is ready (another path completed first)
	if meta, err := readMetadata(m.paths, repo, digestHex); err == nil {
		if meta.Status == StatusReady {
			return nil // Already done
		}
	}

	// Create the digest directory
	dir := digestDir(m.paths, repo, digestHex)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create digest directory: %w", err)
	}

	// Move/copy the pre-built erofs disk to the image store path
	dstPath := digestPath(m.paths, repo, digestHex)
	if err := os.Rename(diskSrcPath, dstPath); err != nil {
		// Rename failed (cross-device), fall back to copy
		srcData, readErr := os.ReadFile(diskSrcPath)
		if readErr != nil {
			return fmt.Errorf("read prebuilt disk: %w", readErr)
		}
		if writeErr := os.WriteFile(dstPath, srcData, 0644); writeErr != nil {
			return fmt.Errorf("write prebuilt disk: %w", writeErr)
		}
	}

	// Align to sector boundary (required by hypervisors, same as convertToErofs in disk.go)
	diskInfo, err := os.Stat(dstPath)
	if err != nil {
		return fmt.Errorf("stat prebuilt disk: %w", err)
	}
	if diskInfo.Size()%sectorSize != 0 {
		alignedSize := alignToSector(diskInfo.Size())
		if err := os.Truncate(dstPath, alignedSize); err != nil {
			return fmt.Errorf("align prebuilt disk to sector boundary: %w", err)
		}
		diskInfo, err = os.Stat(dstPath)
		if err != nil {
			return fmt.Errorf("stat aligned prebuilt disk: %w", err)
		}
	}

	// Extract metadata from OCI cache with retry.
	// The registry push handler populates the OCI cache asynchronously,
	// so it may not be available immediately after the build completes.
	layoutTag := digestToLayoutTag(digestStr)
	var containerMeta *containerMetadata
	for attempt := 0; attempt < 20; attempt++ {
		containerMeta, err = m.ociClient.extractOCIMetadata(layoutTag)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled waiting for OCI metadata: %w", ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	if err != nil {
		// Metadata not available - write with empty metadata rather than failing.
		// The image is still bootable, just without entrypoint/cmd/env info.
		containerMeta = &containerMetadata{
			Env: make(map[string]string),
		}
		fmt.Fprintf(os.Stderr, "Warning: could not extract OCI metadata for prebuilt image %s: %v\n", imageName, err)
	}

	// Write metadata with status=ready
	meta := &imageMetadata{
		Name:       imageName,
		Digest:     digestStr,
		Status:     StatusReady,
		SizeBytes:  diskInfo.Size(),
		Entrypoint: containerMeta.Entrypoint,
		Cmd:        containerMeta.Cmd,
		Env:        containerMeta.Env,
		WorkingDir: containerMeta.WorkingDir,
		CreatedAt:  time.Now(),
	}

	if err := writeMetadata(m.paths, repo, digestHex, meta); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}

	// Create tag symlink (e.g., "latest" -> digest hex)
	// Parse the image name to extract the tag
	normalized, parseErr := ParseNormalizedRef(imageName)
	if parseErr == nil && normalized.Tag() != "" {
		if symlinkErr := createTagSymlink(m.paths, repo, normalized.Tag(), digestHex); symlinkErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to create tag symlink for prebuilt image: %v\n", symlinkErr)
		}
	}

	return nil
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
		fmt.Fprintf(os.Stderr, "image build failed: create build dir: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "image build failed: pull and export %s: %v\n", ref.String(), err)
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
	diskSize, err := ExportRootfs(tempDir, diskPath, DefaultImageFormat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "image build failed: convert to %s for %s: %v\n", DefaultImageFormat, ref.String(), err)
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

// TotalImageBytes returns the total size of all ready images on disk.
func (m *manager) TotalImageBytes(ctx context.Context) (int64, error) {
	images, err := m.ListImages(ctx)
	if err != nil {
		return 0, err
	}

	var total int64
	for _, img := range images {
		if img.Status == StatusReady && img.SizeBytes != nil {
			total += *img.SizeBytes
		}
	}
	return total, nil
}

// TotalOCICacheBytes returns the total size of the OCI layer cache.
// Uses OCI layout metadata instead of walking the filesystem for efficiency.
func (m *manager) TotalOCICacheBytes(ctx context.Context) (int64, error) {
	path, err := layout.FromPath(m.paths.SystemOCICache())
	if err != nil {
		return 0, nil // No cache yet
	}

	index, err := path.ImageIndex()
	if err != nil {
		return 0, nil // Empty or invalid cache
	}

	manifest, err := index.IndexManifest()
	if err != nil {
		return 0, nil
	}

	// Collect unique blob digests and sizes (layers are shared/deduplicated)
	blobSizes := make(map[string]int64)

	for _, desc := range manifest.Manifests {
		// Count the manifest blob itself
		blobSizes[desc.Digest.String()] = desc.Size

		// Get image to access layers and config
		img, err := path.Image(desc.Digest)
		if err != nil {
			continue
		}

		// Count config blob
		if configDigest, err := img.ConfigName(); err == nil {
			if configFile, err := img.RawConfigFile(); err == nil {
				blobSizes[configDigest.String()] = int64(len(configFile))
			}
		}

		// Count layer blobs
		if layers, err := img.Layers(); err == nil {
			for _, layer := range layers {
				if digest, err := layer.Digest(); err == nil {
					if size, err := layer.Size(); err == nil {
						blobSizes[digest.String()] = size
					}
				}
			}
		}
	}

	var total int64
	for _, size := range blobSizes {
		total += size
	}
	return total, nil
}
