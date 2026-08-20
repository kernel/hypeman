package images

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/uuid"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/queue"
	"github.com/kernel/hypeman/lib/tags"
	"go.opentelemetry.io/otel/metric"
)

var errStaleBuild = errors.New("stale image build")

const (
	StatusPending    = "pending"
	StatusPulling    = "pulling"
	StatusConverting = "converting"
	StatusReady      = "ready"
	StatusFailed     = "failed"

	DefaultBorrowedCredentialsTimeout = 30 * time.Minute
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
	// TagImage creates or updates a local tag for an existing image.
	TagImage(ctx context.Context, source, target string) (*Image, error)
	// DeleteImage removes local tags in the reference's repository. Shared
	// content remains available by digest while other local references exist.
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

type inflightImagePull struct {
	fingerprint         [32]byte
	credentials         *authn.AuthConfig
	credentialsExpireAt time.Time
	timer               *time.Timer
}

type manager struct {
	paths                      *paths.Paths
	ociClient                  *ociClient
	queue                      *queue.Queue
	createMu                   sync.Mutex
	diskUsageMu                sync.RWMutex
	diskUsageLoaded            bool
	readyImageBytes            int64
	ociCacheBytes              int64
	metrics                    *Metrics
	inflightPulls              map[string]*inflightImagePull // keyed by digest
	borrowedCredentialsTimeout time.Duration
	readySubscribers           map[string][]chan StatusEvent // keyed by digestHex
	subscriberMu               sync.RWMutex
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
		paths:                      p,
		ociClient:                  ociClient,
		queue:                      queue.New(maxConcurrentBuilds),
		inflightPulls:              make(map[string]*inflightImagePull),
		borrowedCredentialsTimeout: DefaultBorrowedCredentialsTimeout,
		readySubscribers:           make(map[string][]chan StatusEvent),
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

func credentialsPresent(credentials *authn.AuthConfig) bool {
	return credentials != nil && (credentials.Username != "" || credentials.Password != "" || credentials.Auth != "" || credentials.IdentityToken != "" || credentials.RegistryToken != "")
}

func cloneCredentials(credentials *authn.AuthConfig) *authn.AuthConfig {
	if !credentialsPresent(credentials) {
		return nil
	}
	cloned := *credentials
	return &cloned
}

func credentialFingerprint(credentials *authn.AuthConfig) [32]byte {
	if !credentialsPresent(credentials) {
		return [32]byte{}
	}
	return sha256.Sum256([]byte(credentials.Username + "\x00" + credentials.Password + "\x00" + credentials.Auth + "\x00" + credentials.IdentityToken + "\x00" + credentials.RegistryToken))
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
	req.Credentials = cloneCredentials(req.Credentials)
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

	var inspector ManifestInspector = m.ociClient
	if req.Credentials != nil {
		inspector = &credentialedManifestInspector{client: m.ociClient, credentials: req.Credentials}
	}

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
		actual, ref, err = normalized.ResolveDigest(resolveCtx, inspector, platform.ToGCR())
		if err != nil {
			return nil, fmt.Errorf("resolve digest manifest: %w", err)
		}
		if err := validateDigestPlatform(req.Platform, platform, actual); err != nil {
			return nil, err
		}
	} else {
		// inspectManifestWithPlatform already classifies registry errors via
		// ClassifyRegistryError, so just propagate with %w to keep the errors.Is chain.
		ref, err = normalized.ResolveForPlatform(resolveCtx, inspector, platform.ToGCR())
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
			os.RemoveAll(digestDir(m.paths, ref.Repository(), ref.DigestHex()))
			// Fall through to re-queue the build
		} else {
			// We have this digest already (ready, pending, pulling, or converting).
			// last-pull-wins: repoint the tag to it (see createTagSymlink).
			if meta.Status == StatusReady {
				if ref.Tag() != "" {
					createTagSymlink(m.paths, ref.Repository(), ref.Tag(), ref.DigestHex())
				}
				return meta.toImage(), nil
			}
			if !m.inflightCredentialsMatch(ref.Digest(), req.Credentials) {
				return nil, fmt.Errorf("%w: retry after the current pull completes", ErrCredentialConflict)
			}
			img := meta.toImage()
			if meta.Status == StatusPending {
				img.QueuePosition = m.queue.GetPosition(meta.Digest)
			}
			return img, nil
		}
	}

	// Don't have this digest yet, queue the build
	return m.createAndQueueImage(ref, req, platform)
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
			os.RemoveAll(digestDir(m.paths, ref.Repository(), ref.DigestHex()))
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
	return m.createAndQueueImage(ref, CreateImageRequest{Name: imageRef}, hostPlatform())
}

func (m *manager) inflightCredentialsMatch(digest string, credentials *authn.AuthConfig) bool {
	inflight := m.inflightPulls[digest]
	var existingFingerprint [32]byte
	if inflight != nil {
		existingFingerprint = inflight.fingerprint
	}
	return existingFingerprint == credentialFingerprint(credentials)
}

func (m *manager) registerInflightPull(digest string, credentials *authn.AuthConfig) *inflightImagePull {
	if m.inflightPulls == nil {
		m.inflightPulls = make(map[string]*inflightImagePull)
	}
	if previous := m.inflightPulls[digest]; previous != nil && previous.timer != nil {
		previous.timer.Stop()
	}
	inflight := &inflightImagePull{
		fingerprint: credentialFingerprint(credentials),
		credentials: credentials,
	}
	m.inflightPulls[digest] = inflight
	if credentials == nil {
		return inflight
	}
	if m.borrowedCredentialsTimeout <= 0 {
		m.borrowedCredentialsTimeout = DefaultBorrowedCredentialsTimeout
	}
	inflight.credentialsExpireAt = time.Now().Add(m.borrowedCredentialsTimeout)
	inflight.timer = time.AfterFunc(m.borrowedCredentialsTimeout, func() {
		m.createMu.Lock()
		defer m.createMu.Unlock()
		if m.inflightPulls[digest] == inflight {
			inflight.credentials = nil
		}
	})
	return inflight
}

func (m *manager) releaseInflightPull(digest string, inflight *inflightImagePull) func() {
	return func() {
		m.createMu.Lock()
		defer m.createMu.Unlock()
		if m.inflightPulls[digest] != inflight {
			return
		}
		delete(m.inflightPulls, digest)
		if inflight.timer != nil {
			inflight.timer.Stop()
		}
		inflight.credentials = nil
	}
}

func (m *manager) borrowedAuth(digest string, expected *inflightImagePull) (*authn.AuthConfig, time.Time, bool, bool) {
	m.createMu.Lock()
	defer m.createMu.Unlock()
	inflight := m.inflightPulls[digest]
	if expected != nil && inflight != expected {
		return nil, time.Time{}, false, true
	}
	if inflight == nil || inflight.credentialsExpireAt.IsZero() {
		return nil, time.Time{}, false, false
	}
	if inflight.credentials == nil || time.Now().After(inflight.credentialsExpireAt) {
		return nil, inflight.credentialsExpireAt, true, false
	}
	return inflight.credentials, inflight.credentialsExpireAt, false, false
}

func (m *manager) createAndQueueImage(ref *ResolvedRef, req CreateImageRequest, requestedPlatform Platform) (*Image, error) {
	// Build one request value and reuse it for both the persisted metadata and
	// the queue entry so they never diverge.
	storedReq := CreateImageRequest{
		Name:     ref.String(),
		Tags:     tags.Clone(req.Tags),
		Platform: req.Platform,
	}
	// Record the requested platform optimistically while the build is pending;
	// buildImage overwrites it with the authoritative manifest platform once the
	// image config is pulled.
	meta := &imageMetadata{
		Name:         ref.String(),
		Digest:       ref.Digest(),
		Platform:     requestedPlatform.String(),
		Status:       StatusPending,
		Request:      &storedReq,
		BorrowedAuth: req.Credentials != nil,
		BuildID:      uuid.New().String(),
		Tags:         tags.Clone(req.Tags),
		CreatedAt:    time.Now(),
	}

	// Write initial metadata
	if err := writeMetadata(m.paths, ref.Repository(), ref.DigestHex(), meta); err != nil {
		return nil, fmt.Errorf("write initial metadata: %w", err)
	}

	// Keep borrowed credentials outside the queued closure so their lifetime is
	// bounded even when this job waits behind another pull.
	inflight := m.registerInflightPull(ref.Digest(), req.Credentials)
	buildID := meta.BuildID
	queuePos := m.queue.EnqueueSuccessor(ref.Digest(), func() {
		credentials, deadline, expired, stale := m.borrowedAuth(ref.Digest(), inflight)
		if stale {
			return
		}
		if expired {
			m.updateStatusByDigest(ref, StatusFailed, ErrBorrowedCredentialsExpired, buildID)
			return
		}
		ctx := context.Background()
		if !deadline.IsZero() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, deadline)
			defer cancel()
		}
		m.buildImage(ctx, ref, credentials, buildID)
	}, m.releaseInflightPull(ref.Digest(), inflight))

	img := meta.toImage()
	if queuePos > 0 {
		img.QueuePosition = &queuePos
	}
	return img, nil
}

func (m *manager) buildImage(ctx context.Context, ref *ResolvedRef, credentials *authn.AuthConfig, buildID string) {
	buildStart := time.Now()
	buildStatus := "failed"
	buildDir := m.paths.SystemBuild(ref.String())
	tempDir := filepath.Join(buildDir, "rootfs")
	defer func() {
		m.recordBuildMetrics(ctx, buildStart, buildStatus)
	}()

	if err := os.MkdirAll(buildDir, 0755); err != nil {
		m.updateStatusByDigest(ref, StatusFailed, fmt.Errorf("create build dir: %w", err), buildID)
		return
	}

	defer func() {
		// Clean up build directory after completion
		start := time.Now()
		err := os.RemoveAll(buildDir)
		m.recordImageBuildPhase(ctx, ref.Digest(), "cleanup", time.Since(start), phaseStatus(err), "not_applicable")
	}()

	m.updateStatusByDigest(ref, StatusPulling, nil, buildID)

	// Pull by the digest-pinned reference, not the tag: a digest ref fetches the
	// exact manifest regardless of the platform passed downstream, so a
	// non-host-arch image (tag preserved in ref.String()/ref.Tag() for symlinks)
	// still pulls the architecture its digest identifies. Uses the cache if the
	// digest is already pulled.
	pullRef := ref.DigestRef()
	result, err := m.ociClient.pullAndExportWithAuth(ctx, pullRef, ref.Digest(), tempDir, credentials)
	m.recordPullResultMetrics(ctx, ref.Digest(), result)
	if err != nil {
		m.updateStatusByDigest(ref, StatusFailed, fmt.Errorf("pull and export: %w", err), buildID)
		m.recordPullMetrics(ctx, "failed")
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
			buildStatus = "success"
			return
		}
	}

	m.updateStatusByDigest(ref, StatusConverting, nil, buildID)

	diskPath := resolveImageLayout(m.paths, ref.Repository(), ref.DigestHex()).disk
	// Use default image format (erofs on Linux, ext4 on Darwin)
	convertStart := time.Now()
	diskSize, err := ExportRootfs(tempDir, diskPath, DefaultImageFormat)
	m.recordImageBuildPhase(ctx, ref.Digest(), "filesystem_export", time.Since(convertStart), phaseStatus(err), "not_applicable")
	if err != nil {
		m.updateStatusByDigest(ref, StatusFailed, fmt.Errorf("convert to %s: %w", DefaultImageFormat, err), buildID)
		return
	}

	finalizeStart := time.Now()
	err = m.finalizeImage(ref, result, diskSize, buildID)
	m.recordImageBuildPhase(ctx, ref.Digest(), "finalize", time.Since(finalizeStart), phaseStatus(err), "not_applicable")
	if err != nil {
		if errors.Is(err, errStaleBuild) {
			return
		}
		m.updateStatusByDigest(ref, StatusFailed, err, buildID)
		return
	}

	buildStatus = "success"
}

func (m *manager) finalizeImage(ref *ResolvedRef, result *pullResult, diskSize int64, buildID string) error {
	m.createMu.Lock()
	defer m.createMu.Unlock()

	// Read current metadata to preserve request info and reject stale builds.
	meta, err := readMetadata(m.paths, ref.Repository(), ref.DigestHex())
	if err != nil || meta.BuildID != buildID {
		return errStaleBuild
	}

	// The pulled image config is the source of truth for the platform.
	var requestedPlatform string
	if meta.Request != nil {
		requestedPlatform = meta.Request.Platform
	}
	actualPlatform, err := resolveManifestPlatform(result.Metadata, requestedPlatform)
	if err != nil {
		return err
	}

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
		return fmt.Errorf("write final metadata: %w", err)
	}

	m.notifyReady(ref.DigestHex(), StatusReady, nil)
	if ref.Tag() != "" {
		if err := createTagSymlink(m.paths, ref.Repository(), ref.Tag(), ref.DigestHex()); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to create tag symlink: %v\n", err)
		}
	}
	m.refreshDiskUsageTotals()
	return nil
}

func phaseStatus(err error) string {
	if err != nil {
		return "failed"
	}
	return "success"
}

func (m *manager) recordPullResultMetrics(ctx context.Context, digest string, result *pullResult) {
	if result == nil {
		return
	}
	cacheStatus := "miss"
	if result.CacheHit {
		cacheStatus = "hit"
	}
	for _, phase := range result.Phases {
		m.recordImageBuildPhase(ctx, digest, phase.Phase, phase.Duration, phase.Status, cacheStatus)
	}
	if result.Metadata != nil {
		m.recordOCIImageMetrics(ctx, result.LayerCount, result.CompressedBytes, cacheStatus)
		slog.InfoContext(ctx, "OCI image inspected",
			"digest", digest,
			"cache_status", cacheStatus,
			"layer_count", result.LayerCount,
			"compressed_bytes", result.CompressedBytes,
		)
	}
}

func (m *manager) recordImageBuildPhase(ctx context.Context, digest, phase string, duration time.Duration, status, cacheStatus string) {
	m.recordBuildPhaseMetrics(ctx, phase, duration, status, cacheStatus)
	slog.InfoContext(ctx, "image build phase completed",
		"digest", digest,
		"phase", phase,
		"status", status,
		"cache_status", cacheStatus,
		"duration_seconds", duration.Seconds(),
	)
}

func (m *manager) updateStatusByDigest(ref *ResolvedRef, status string, err error, buildID string) {
	m.createMu.Lock()
	defer m.createMu.Unlock()

	meta, readErr := readMetadata(m.paths, ref.Repository(), ref.DigestHex())
	if readErr != nil || meta.BuildID != buildID {
		return
	}
	meta.Status = status

	if err != nil {
		errorMsg := err.Error()
		meta.Error = &errorMsg
	}

	writeMetadata(m.paths, ref.Repository(), ref.DigestHex(), meta)

	// Notify while holding createMu so a delete/recreate cannot race the
	// metadata write and receive a terminal event for the old build.
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
		if meta.Status != StatusPending && meta.Status != StatusPulling && meta.Status != StatusConverting {
			continue
		}
		if meta.Request == nil || meta.Digest == "" {
			continue
		}
		normalized, err := ParseNormalizedRef(meta.Name)
		if err != nil {
			continue
		}
		ref := NewResolvedRef(normalized, meta.Digest)
		if meta.BorrowedAuth && (meta.Status != StatusConverting || !m.ociClient.existsInLayout(digestToLayoutTag(meta.Digest))) {
			m.updateStatusByDigest(ref, StatusFailed, ErrBorrowedCredentialsExpired, meta.BuildID)
			continue
		}
		buildID := meta.BuildID
		m.queue.Enqueue(meta.Digest, func() {
			m.buildImage(context.Background(), ref, nil, buildID)
		}, nil)
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
	if !ref.IsDigest() {
		img.Name = ref.String()
	}

	if meta.Status == StatusPending {
		img.QueuePosition = m.queue.GetPosition(meta.Digest)
	}

	return img, nil
}

// TagImage creates or updates target as a local tag for source without pulling
// the image again. Source may be a tag or digest reference; target must be a tag.
func (m *manager) TagImage(ctx context.Context, source, target string) (*Image, error) {
	sourceRef, err := ParseNormalizedRef(source)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid source reference: %s", ErrInvalidName, err)
	}
	targetRef, err := ParseNormalizedRef(target)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid target reference: %s", ErrInvalidName, err)
	}
	if targetRef.IsDigest() {
		return nil, fmt.Errorf("%w: target must include a tag", ErrInvalidName)
	}
	m.createMu.Lock()
	defer m.createMu.Unlock()

	digestHex := sourceRef.DigestHex()
	if !sourceRef.IsDigest() {
		digestHex, err = resolveTag(m.paths, sourceRef.Repository(), sourceRef.Tag())
		if err != nil {
			return nil, err
		}
	}

	meta, err := readMetadata(m.paths, sourceRef.Repository(), digestHex)
	if err != nil {
		return nil, err
	}
	if meta.Status != StatusReady {
		return nil, fmt.Errorf("%w: %s", ErrImageNotReady, meta.Status)
	}
	if sourceRef.Repository() != targetRef.Repository() {
		if err := promoteImageToContent(m.paths, sourceRef.Repository(), digestHex, meta); err != nil {
			return nil, fmt.Errorf("create image alias: %w", err)
		}
	}
	if err := createTagSymlink(m.paths, targetRef.Repository(), targetRef.Tag(), digestHex); err != nil {
		return nil, fmt.Errorf("create image tag: %w", err)
	}

	img := *meta.toImage()
	img.Name = targetRef.String()
	return &img, nil
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
		if err := removeDigestIfUnreferenced(m.paths, repository, digestHex, false); err != nil {
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
		if err := removeDigestIfUnreferenced(m.paths, repository, digestHex, true); err != nil {
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
		return conversionFailedErr(img.Error, nil)
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
			return conversionFailedErr(img.Error, nil)
		}
	}

	// Wait for notification or context cancellation
	select {
	case event := <-ch:
		if event.Status == StatusReady {
			return nil
		}
		return conversionFailedErr(nil, event.Err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func conversionFailedErr(message *string, cause error) error {
	if cause != nil {
		return fmt.Errorf("image conversion failed: %w", cause)
	}
	if message != nil {
		return fmt.Errorf("image conversion failed: %s", *message)
	}
	return errors.New("image conversion failed")
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
