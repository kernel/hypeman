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

	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/tags"
	"go.opentelemetry.io/otel/metric"
)

const (
	StatusPending    = "pending"
	StatusPulling    = "pulling"
	StatusConverting = "converting"
	StatusReady      = "ready"
	StatusFailed     = "failed"
)

// StatusEvent represents a terminal status change for image readiness notifications.
type StatusEvent struct {
	Status string
	Err    error
}

type Manager interface {
	ListImages(ctx context.Context) ([]Image, error)
	CreateImage(ctx context.Context, req CreateImageRequest) (*Image, error)
	// ImportLocalImage imports an image that was pushed to the local OCI cache.
	// Unlike CreateImage, it does not resolve from a remote registry.
	ImportLocalImage(ctx context.Context, repo, reference, digest string) (*Image, error)
	GetImage(ctx context.Context, name string) (*Image, error)
	DeleteImage(ctx context.Context, name string) error
	RecoverInterruptedBuilds()
	// TotalImageBytes returns the total size of all ready images on disk.
	// Used by the resource manager for disk capacity tracking.
	TotalImageBytes(ctx context.Context) (int64, error)
	// TotalOCICacheBytes returns the total size of the OCI layer cache.
	// Used by the resource manager for disk capacity tracking.
	TotalOCICacheBytes(ctx context.Context) (int64, error)
	// WaitForReady blocks until the image identified by name reaches a terminal
	// state (ready or failed) or the context is cancelled.
	WaitForReady(ctx context.Context, name string) error
}

type manager struct {
	paths            *paths.Paths
	ociClient        *ociClient
	queue            *BuildQueue
	createMu         sync.Mutex
	diskUsageMu      sync.RWMutex
	diskUsageLoaded  bool
	readyImageBytes  int64
	ociCacheBytes    int64
	metrics          *Metrics
	readySubscribers map[string][]chan StatusEvent // keyed by digestHex
	subscriberMu     sync.RWMutex
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
		paths:            p,
		ociClient:        ociClient,
		queue:            NewBuildQueue(maxConcurrentBuilds),
		readySubscribers: make(map[string][]chan StatusEvent),
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
	metas, err := listAllMetadata(m.paths)
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}

	images := make([]Image, 0, len(metas))
	for _, meta := range metas {
		images = append(images, *meta.toImage())
	}

	return images, nil
}

func (m *manager) CreateImage(ctx context.Context, req CreateImageRequest) (*Image, error) {
	if err := tags.Validate(req.Tags); err != nil {
		return nil, err
	}

	// Validate the requested platform up front so typos fail fast before any
	// registry round-trip.
	platform, err := resolveRequestPlatform(req.Platform)
	if err != nil {
		return nil, err
	}

	// Parse and normalize
	normalized, err := ParseNormalizedRef(req.Name)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidName, err.Error())
	}

	// Resolve to get digest (validates existence)
	// Add a 5-second timeout to ensure fast failure on rate limits or errors
	resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var ref *ResolvedRef
	if normalized.IsDigest() {
		// A digest pin must resolve to its exact manifest (verifying the digest
		// exists: a bogus digest -> 404 instead of an aliased "ready" entry) and,
		// when an index is pinned, to the child for the requested platform.
		// ResolveDigest returns the resolved image's real os/arch (to validate any
		// explicit --platform) and a ref pinned to the resolved child digest. The
		// resolve goes through the ManifestInspector seam, symmetric with the tag
		// path below, so it stays fake-testable.
		var actual Platform
		actual, ref, err = normalized.ResolveDigest(resolveCtx, m.ociClient, platform.ToGCR())
		if err != nil {
			return nil, fmt.Errorf("resolve digest manifest: %w", err)
		}
		if err := validateDigestPlatform(req.Platform, platform, actual); err != nil {
			return nil, err
		}
	} else {
		// inspectManifestWithPlatform already classifies registry errors via
		// ClassifyRegistryError, so just propagate with %w to keep the errors.Is chain.
		ref, err = normalized.ResolveForPlatform(resolveCtx, m.ociClient, platform.ToGCR())
		if err != nil {
			return nil, fmt.Errorf("resolve manifest for platform %s: %w", platform, err)
		}
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
			// We have this digest already (ready, pending, pulling, or converting).
			// last-pull-wins: repoint the tag to it (see createTagSymlink).
			if meta.Status == StatusReady && ref.Tag() != "" {
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
	return m.createAndQueueImage(ref, req)
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

	// Create a ResolvedRef directly with the provided digest. Unlike CreateImage's
	// digest path (which calls ResolveDigest to verify the digest exists in the
	// registry), the digest here references a blob just pushed to the local OCI
	// cache, so it is trusted and not remotely re-resolved.
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
			// We have this digest already; last-pull-wins repoints the tag.
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
	return m.createAndQueueImage(ref, CreateImageRequest{Name: imageRef})
}

func (m *manager) createAndQueueImage(ref *ResolvedRef, req CreateImageRequest) (*Image, error) {
	// Build one request value and reuse it for both the persisted metadata and
	// the queue entry so they never diverge.
	storedReq := CreateImageRequest{
		Name:     ref.String(),
		Tags:     tags.Clone(req.Tags),
		Platform: req.Platform,
	}
	// Record the requested platform optimistically while the build is pending;
	// buildImage overwrites it with the authoritative manifest platform once the
	// image config is pulled. resolveRequestPlatform already validated req, so
	// the error here is unreachable in practice.
	// TODO(followup) kernel/hypeman#283: pass the already-parsed platform from
	// CreateImage instead of re-resolving here (also removes this dead error path).
	requestedPlatform, err := resolveRequestPlatform(req.Platform)
	if err != nil {
		return nil, err
	}
	meta := &imageMetadata{
		Name:      ref.String(),
		Digest:    ref.Digest(),
		Platform:  requestedPlatform.String(),
		Status:    StatusPending,
		Request:   &storedReq,
		Tags:      tags.Clone(req.Tags),
		CreatedAt: time.Now(),
	}

	// Write initial metadata
	if err := writeMetadata(m.paths, ref.Repository(), ref.DigestHex(), meta); err != nil {
		return nil, fmt.Errorf("write initial metadata: %w", err)
	}

	// Enqueue the build using digest as the queue key for deduplication
	queuePos := m.queue.Enqueue(ref.Digest(), storedReq, func() {
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

	// Pull by the digest-pinned reference, not the tag: a digest ref fetches the
	// exact manifest regardless of the platform passed downstream, so a
	// non-host-arch image (tag preserved in ref.String()/ref.Tag() for symlinks)
	// still pulls the architecture its digest identifies. Uses the cache if the
	// digest is already pulled.
	pullRef := ref.Repository() + "@" + ref.Digest()
	result, err := m.ociClient.pullAndExport(ctx, pullRef, ref.Digest(), tempDir)
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
			// Another build completed first; last-pull-wins repoints the tag.
			if ref.Tag() != "" {
				createTagSymlink(m.paths, ref.Repository(), ref.Tag(), ref.DigestHex())
			}
			return
		}
	}

	m.updateStatusByDigest(ref, StatusConverting, nil)

	diskPath := digestPath(m.paths, ref.Repository(), ref.DigestHex())
	// Use default image format (erofs on Linux, ext4 on Darwin)
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

	// The pulled image config is the source of truth for the platform. Record
	// it rather than the requested value so the boot guard trusts a real
	// platform (WithPlatform is a no-op for single-manifest images, so a wrong
	// request would otherwise be silently recorded).
	var requestedPlatform string
	if meta.Request != nil {
		requestedPlatform = meta.Request.Platform
	}
	actualPlatform, err := resolveManifestPlatform(result.Metadata, requestedPlatform)
	if err != nil {
		m.updateStatusByDigest(ref, StatusFailed, err)
		return
	}

	// Update with final status
	meta.Status = StatusReady
	meta.Error = nil
	meta.Platform = actualPlatform.String()
	meta.SizeBytes = diskSize
	meta.Entrypoint = result.Metadata.Entrypoint
	meta.Cmd = result.Metadata.Cmd
	meta.Env = result.Metadata.Env
	meta.Labels = result.Metadata.Labels
	meta.WorkingDir = result.Metadata.WorkingDir

	if err := writeMetadata(m.paths, ref.Repository(), ref.DigestHex(), meta); err != nil {
		m.updateStatusByDigest(ref, StatusFailed, fmt.Errorf("write final metadata: %w", err))
		return
	}

	// Notify subscribers that image is ready
	m.notifyReady(ref.DigestHex(), StatusReady, nil)

	// Only create/update tag symlink on successful completion; last-pull-wins
	// repoints the tag regardless of platform.
	if ref.Tag() != "" {
		if err := createTagSymlink(m.paths, ref.Repository(), ref.Tag(), ref.DigestHex()); err != nil {
			// Log error but don't fail the build
			fmt.Fprintf(os.Stderr, "Warning: failed to create tag symlink: %v\n", err)
		}
	}

	m.refreshDiskUsageTotals()

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

	// Notify subscribers of terminal status
	if status == StatusReady || status == StatusFailed {
		m.notifyReady(ref.DigestHex(), status, err)
	}
}

func (m *manager) RecoverInterruptedBuilds() {
	metas, err := listAllMetadata(m.paths)
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
		d, err := resolveTag(m.paths, repository, tag)
		if err != nil {
			return nil, err
		}
		digestHex = d
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

	repository := ref.Repository()

	// Hold createMu during delete so tag resolution, tag removal, and orphan checks
	// stay consistent with concurrent creates for the same repository digest.
	m.createMu.Lock()
	defer m.createMu.Unlock()

	if ref.IsDigest() {
		digestHex := ref.DigestHex()
		if _, err := readMetadata(m.paths, repository, digestHex); err != nil {
			return err
		}
		if err := deleteTagsForDigest(m.paths, repository, digestHex); err != nil {
			return err
		}
		if err := deleteDigest(m.paths, repository, digestHex); err != nil {
			return err
		}
		m.refreshDiskUsageTotals()
		return nil
	}

	tag := ref.Tag()

	// Resolve the tag to get the digest before deleting
	digestHex, err := resolveTag(m.paths, repository, tag)
	if err != nil {
		return err
	}

	// Delete the tag symlink
	if err := deleteTag(m.paths, repository, tag); err != nil {
		return err
	}

	// Check if the digest is now orphaned (no other tags reference it)
	count, err := countTagsForDigest(m.paths, repository, digestHex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to count tags for digest %s: %v\n", digestHex, err)
		return nil
	}

	if count == 0 {
		// Digest is orphaned, delete it
		if err := deleteDigest(m.paths, repository, digestHex); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to delete orphaned digest %s: %v\n", digestHex, err)
			return nil
		}
		m.refreshDiskUsageTotals()
	}

	return nil
}

// TotalImageBytes returns the total size of all ready images on disk.
func (m *manager) TotalImageBytes(ctx context.Context) (int64, error) {
	readyImageBytes, _, err := m.getDiskUsageTotals()
	if err != nil {
		return 0, err
	}
	return readyImageBytes, nil
}

// TotalOCICacheBytes returns the total size of the OCI layer cache.
func (m *manager) TotalOCICacheBytes(ctx context.Context) (int64, error) {
	_, ociCacheBytes, err := m.getDiskUsageTotals()
	if err != nil {
		return 0, err
	}
	return ociCacheBytes, nil
}

// WaitForReady blocks until the image reaches a terminal state (ready or failed)
// or the context is cancelled.
//
// The image may not exist yet when this is called (e.g., the registry's
// triggerConversion goroutine hasn't called ImportLocalImage yet), so we
// poll briefly for the image to appear before subscribing for notifications.
func (m *manager) WaitForReady(ctx context.Context, name string) error {
	// Wait for the image to appear in the store. In the build flow, the
	// registry triggers ImportLocalImage asynchronously after a push, so the
	// image may not exist when the build manager calls WaitForReady.
	const maxWaitForExist = 30 * time.Second
	const pollInterval = 100 * time.Millisecond

	var img *Image
	deadline := time.Now().Add(maxWaitForExist)
	for {
		got, err := m.GetImage(ctx, name)
		if err == nil {
			img = got
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("get image: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}

	// Check if already in terminal state
	switch img.Status {
	case StatusReady:
		return nil
	case StatusFailed:
		if img.Error != nil {
			return fmt.Errorf("image conversion failed: %s", *img.Error)
		}
		return fmt.Errorf("image conversion failed")
	}

	digestHex := strings.TrimPrefix(img.Digest, "sha256:")

	// Subscribe BEFORE re-checking to avoid TOCTOU race
	ch := make(chan StatusEvent, 1)
	m.subscribeToReady(digestHex, ch)
	defer m.unsubscribeFromReady(digestHex, ch)

	// Re-check after subscribing to close the race window
	img, err := m.GetImage(ctx, name)
	if err == nil {
		switch img.Status {
		case StatusReady:
			return nil
		case StatusFailed:
			if img.Error != nil {
				return fmt.Errorf("image conversion failed: %s", *img.Error)
			}
			return fmt.Errorf("image conversion failed")
		}
	}

	// Wait for notification or context cancellation
	select {
	case event := <-ch:
		if event.Status == StatusReady {
			return nil
		}
		if event.Err != nil {
			return fmt.Errorf("image conversion failed: %w", event.Err)
		}
		return fmt.Errorf("image conversion failed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// subscribeToReady registers a channel for terminal status notifications on a digest.
func (m *manager) subscribeToReady(digestHex string, ch chan StatusEvent) {
	m.subscriberMu.Lock()
	defer m.subscriberMu.Unlock()
	m.readySubscribers[digestHex] = append(m.readySubscribers[digestHex], ch)
}

// unsubscribeFromReady removes a subscriber channel.
func (m *manager) unsubscribeFromReady(digestHex string, ch chan StatusEvent) {
	m.subscriberMu.Lock()
	defer m.subscriberMu.Unlock()

	subscribers := m.readySubscribers[digestHex]
	for i, sub := range subscribers {
		if sub == ch {
			m.readySubscribers[digestHex] = append(subscribers[:i], subscribers[i+1:]...)
			break
		}
	}

	if len(m.readySubscribers[digestHex]) == 0 {
		delete(m.readySubscribers, digestHex)
	}
}

// notifyReady broadcasts a terminal status event to all subscribers for a digest.
func (m *manager) notifyReady(digestHex string, status string, err error) {
	m.subscriberMu.RLock()
	defer m.subscriberMu.RUnlock()

	event := StatusEvent{Status: status, Err: err}
	for _, ch := range m.readySubscribers[digestHex] {
		// Non-blocking send — drop if channel is full
		select {
		case ch <- event:
		default:
		}
	}
}
