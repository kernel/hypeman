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
	"golang.org/x/sync/singleflight"
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
	// TagImage creates or updates a local tag pointing at an existing ready
	// image, without pulling or reconverting. Source and target may be in
	// different repositories.
	TagImage(ctx context.Context, source, target string) (*Image, error)
	DeleteImage(ctx context.Context, name string) error
	RecoverInterruptedBuilds()
	// TotalImageBytes returns the total size of all ready images on disk.
	// Used by the resource manager for disk capacity tracking.
	TotalImageBytes(ctx context.Context) (int64, error)
	// TotalOCICacheBytes returns the total size of the OCI and materialized layer caches.
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
	layerFlights               singleflight.Group
	diskUsageMu                sync.RWMutex
	tagGenerations             map[string]uint64
	requestedTags              map[string]string // newest pull's digest per requested tag
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
		tagGenerations:             make(map[string]uint64),
		requestedTags:              make(map[string]string),
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
	// Keep legacy images readable in their existing layout and promote them only
	// when an operation needs shared content, such as a cross-repository tag.
	// Avoiding a startup-wide migration keeps startup bounded and independent of
	// migration success.
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
		images = append(images, *meta.toImageFor(meta.Name))
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

	if img, found, err := m.reuseExistingImage(ref, req.Credentials, req.Tags); found || err != nil {
		return img, err
	}
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

	if img, found, err := m.reuseExistingImage(ref, nil, nil); found || err != nil {
		return img, err
	}
	return m.createAndQueueImage(ref, CreateImageRequest{Name: imageRef}, hostPlatform())
}

func (m *manager) reuseExistingImage(ref *ResolvedRef, credentials *authn.AuthConfig, resourceTags tags.Tags) (*Image, bool, error) {
	meta, err := readMetadata(m.paths, ref.Repository(), ref.DigestHex())
	if err != nil {
		return nil, false, nil
	}
	if meta.Status == StatusFailed {
		if err := removeDigestIfUnreferenced(m.paths, ref.Repository(), ref.DigestHex(), false); err != nil {
			return nil, true, fmt.Errorf("remove failed image: %w", err)
		}
		return nil, false, nil
	}
	if meta.Status != StatusReady {
		// A pending pull with different credentials does not get to point
		// the tag at its digest or update its labels.
		if !m.inflightCredentialsMatch(ref.Digest(), credentials) {
			return nil, true, fmt.Errorf("%w: retry after the current pull completes", ErrCredentialConflict)
		}
	}
	if resourceTags != nil {
		meta.Tags = tags.Clone(resourceTags)
		if err := writeMetadata(m.paths, ref.Repository(), ref.DigestHex(), meta); err != nil {
			return nil, true, fmt.Errorf("update image tags: %w", err)
		}
	}
	if ref.Tag() != "" {
		if err := m.claimTagForStatus(meta, ref); err != nil {
			return nil, true, fmt.Errorf("create image tag: %w", err)
		}
	}
	img := meta.toImageFor(ref.String())
	if meta.Status == StatusPending {
		img.QueuePosition = m.queue.GetPosition(meta.Digest)
	}
	return img, true, nil
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
	meta, previousTagDigest, err := m.newPendingImageMetadata(ref, req, requestedPlatform)
	if err != nil {
		return nil, err
	}

	// Write initial metadata
	if err := writeMetadata(m.paths, ref.Repository(), ref.DigestHex(), meta); err != nil {
		if ref.Tag() != "" {
			m.revertTagGeneration(ref.Repository(), ref.Tag())
			m.clearRequestedTag(ref.Repository(), ref.Tag(), ref.DigestHex())
		}
		return nil, fmt.Errorf("write initial metadata: %w", err)
	}
	if ref.Tag() != "" && previousTagDigest == "" {
		if err := createTagSymlink(m.paths, ref.Repository(), ref.Tag(), ref.DigestHex()); err != nil {
			m.revertTagGeneration(ref.Repository(), ref.Tag())
			m.clearRequestedTag(ref.Repository(), ref.Tag(), ref.DigestHex())
			return nil, fmt.Errorf("create pending image tag: %w", err)
		}
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

	img := meta.toImageFor(ref.String())
	if queuePos > 0 {
		img.QueuePosition = &queuePos
	}
	return img, nil
}

func (m *manager) newPendingImageMetadata(ref *ResolvedRef, req CreateImageRequest, requestedPlatform Platform) (*imageMetadata, string, error) {
	previousTagDigest := ""
	if ref.Tag() != "" {
		var err error
		previousTagDigest, err = resolveTag(m.paths, ref.Repository(), ref.Tag())
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, "", fmt.Errorf("resolve existing image tag: %w", err)
		}
	}
	tagGeneration := uint64(0)
	if ref.Tag() != "" {
		tagGeneration = m.nextTagGeneration(ref.Repository(), ref.Tag())
		m.trackRequestedTag(ref.Repository(), ref.Tag(), ref.DigestHex())
	}
	storedReq := CreateImageRequest{
		Name:     ref.String(),
		Tags:     tags.Clone(req.Tags),
		Platform: req.Platform,
	}
	return &imageMetadata{
		Name:              ref.String(),
		Digest:            ref.Digest(),
		Platform:          requestedPlatform.String(),
		Status:            StatusPending,
		Request:           &storedReq,
		BorrowedAuth:      req.Credentials != nil,
		BuildID:           uuid.New().String(),
		Tags:              tags.Clone(req.Tags),
		RequestedTag:      ref.Tag(),
		PreviousTagDigest: previousTagDigest,
		TagGeneration:     tagGeneration,
		CreatedAt:         time.Now(),
	}, previousTagDigest, nil
}

func (m *manager) buildImage(ctx context.Context, ref *ResolvedRef, credentials *authn.AuthConfig, buildID string) {
	buildStart := time.Now()
	buildStatus := "failed"
	// Key the build directory by digest so two pending builds of the same
	// ref with different digests never compose into (and delete) the same
	// rootfs. This matches the queue's digest-based deduplication.
	buildDir := m.paths.SystemBuild(ref.DigestHex())
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
				m.createMu.Lock()
				err := m.claimReadyTag(ref.Repository(), ref.Tag(), ref.DigestHex())
				m.createMu.Unlock()
				if err != nil {
					slog.Warn("failed to claim ready image tag", "repository", ref.Repository(), "tag", ref.Tag(), "error", err)
				}
			}
			buildStatus = "success"
			return
		}
	}

	m.updateStatusByDigest(ref, StatusConverting, nil, buildID)

	diskPath := resolveImageLayout(m.paths, ref.Repository(), ref.DigestHex()).disk
	// Keep the temporary filesystem beside its final path so finalization stays
	// atomic even when system/builds and images are on different filesystems.
	diskTempPath := diskPath + ".tmp-" + buildID
	defer os.Remove(diskTempPath)
	// Use default image format (erofs on Linux, ext4 on Darwin)
	convertStart := time.Now()
	diskSize, err := ExportRootfs(tempDir, diskTempPath, DefaultImageFormat)
	m.recordImageBuildPhase(ctx, ref.Digest(), "filesystem_export", time.Since(convertStart), phaseStatus(err), "not_applicable")
	if err != nil {
		m.updateStatusByDigest(ref, StatusFailed, fmt.Errorf("convert to %s: %w", DefaultImageFormat, err), buildID)
		return
	}

	finalizeStart := time.Now()
	err = m.finalizeImage(ref, result, diskSize, buildID, diskTempPath)
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

func (m *manager) finalizeImage(ref *ResolvedRef, result *pullResult, diskSize int64, buildID, diskTempPath string) error {
	m.createMu.Lock()
	defer m.createMu.Unlock()

	layout := resolveImageLayout(m.paths, ref.Repository(), ref.DigestHex())

	// Read current metadata to preserve request info and reject stale builds.
	meta, err := readMetadataAt(layout)
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

	modelPath := manifestModelPath(m.paths, layout, ref.DigestHex())
	diskInstalled := false
	modelWritten := false

	if err := installAtomically(layout.disk, func(path string) error {
		return os.Rename(diskTempPath, path)
	}); err != nil {
		return fmt.Errorf("install image disk: %w", err)
	}
	diskInstalled = true

	// Persist the manifest content model beside the shared content so later
	// stages can recompose the image from per-layer artifacts and GC can tell
	// which OCI blobs are still referenced.
	if result.Manifest != nil {
		model := *result.Manifest
		model.Platform = actualPlatform.String()
		if err := writeManifestModelAt(modelPath, ref.DigestHex(), &model); err != nil {
			return rollbackFinalization(layout, modelPath, diskInstalled, modelWritten, fmt.Errorf("write manifest model: %w", err))
		}
		modelWritten = true
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

	if err := writeMetadataFile(layout.metadata, meta); err != nil {
		return rollbackFinalization(layout, modelPath, diskInstalled, modelWritten, fmt.Errorf("write final metadata: %w", err))
	}

	m.notifyReady(ref.DigestHex(), StatusReady, nil)
	if !m.claimRequestedTags(ref, meta) {
		m.cleanupUnclaimedImage(ref)
	}
	m.refreshDiskUsageTotals()
	return nil
}

func manifestModelPath(p *paths.Paths, layout imageLayout, digestHex string) string {
	if layout.content {
		return p.ImageContentManifestModel(digestHex)
	}
	return filepath.Join(layout.dir, "manifest.json")
}

func rollbackFinalization(layout imageLayout, modelPath string, diskInstalled, modelWritten bool, cause error) error {
	var rollbackErr error
	if modelWritten {
		rollbackErr = errors.Join(rollbackErr, os.Remove(modelPath))
	}
	if diskInstalled {
		rollbackErr = errors.Join(rollbackErr, os.Remove(layout.disk))
	}
	if rollbackErr != nil {
		return errors.Join(cause, fmt.Errorf("rollback finalization: %w", rollbackErr))
	}
	return cause
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

	layout := resolveImageLayout(m.paths, ref.Repository(), ref.DigestHex())
	meta, readErr := readMetadataAt(layout)
	if readErr != nil || meta.BuildID != buildID {
		return
	}
	meta.Status = status

	if err != nil {
		errorMsg := err.Error()
		meta.Error = &errorMsg
	}

	writeMetadataFile(layout.metadata, meta)

	// Notify while holding createMu so a delete/recreate cannot race the
	// metadata write and receive a terminal event for the old build.
	if status == StatusReady || status == StatusFailed {
		m.notifyReady(ref.DigestHex(), status, err)
	}
	// A failed pull releases its tag claim so an older in-flight pull of the
	// same tag can still repoint it.
	if status == StatusFailed {
		if meta.RequestedTag != "" {
			m.releaseTagGeneration(ref.Repository(), meta.RequestedTag, meta.TagGeneration)
		}
		for _, claim := range meta.TagClaims {
			m.releaseTagGeneration(claim.Repository, claim.Tag, claim.TagGeneration)
		}
		m.clearRequestedDigest(meta.digestHex())
	}
}

func (m *manager) RecoverInterruptedBuilds() {
	metas, err := listAllMetadata(m.paths)
	if err != nil {
		return // Best effort
	}

	sort.Slice(metas, func(i, j int) bool {
		return metas[i].CreatedAt.Before(metas[j].CreatedAt)
	})
	m.restoreTagState(metas)

	seenDigests := make(map[string]struct{})
	for _, meta := range metas {
		if _, seen := seenDigests[meta.Digest]; seen {
			continue
		}
		if m.recoverInterruptedBuild(meta) {
			seenDigests[meta.Digest] = struct{}{}
		}
	}
}

func (m *manager) recoverInterruptedBuild(meta *imageMetadata) bool {
	if !isPendingImageStatus(meta.Status) {
		return false
	}
	if meta.Request == nil || meta.Digest == "" {
		return false
	}
	normalized, err := ParseNormalizedRef(meta.Name)
	if err != nil {
		return false
	}
	ref := NewResolvedRef(normalized, meta.Digest)
	if meta.BorrowedAuth && (meta.Status != StatusConverting || !m.ociClient.existsInLayout(digestToLayoutTag(meta.Digest))) {
		m.updateStatusByDigest(ref, StatusFailed, ErrBorrowedCredentialsExpired, meta.BuildID)
		return true
	}
	buildID := meta.BuildID
	m.queue.Enqueue(meta.Digest, func() {
		m.buildImage(context.Background(), ref, nil, buildID)
	}, nil)
	return true
}

func (m *manager) GetImage(ctx context.Context, name string) (*Image, error) {
	// Parse and normalize the reference
	ref, err := ParseNormalizedRef(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidName, err.Error())
	}

	_, meta, err := resolveRefMetadata(m.paths, ref)
	if err != nil {
		return nil, err
	}

	img := meta.toImageFor(ref.String())

	if meta.Status == StatusPending {
		img.QueuePosition = m.queue.GetPosition(meta.Digest)
	}

	return img, nil
}

func (m *manager) DeleteImage(ctx context.Context, name string) error {
	ref, err := ParseNormalizedRef(name)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidName, err.Error())
	}

	m.createMu.Lock()
	defer m.createMu.Unlock()

	if ref.IsDigest() {
		return m.deleteDigestImage(ref.Repository(), ref.DigestHex())
	}
	return m.deleteTaggedImage(ref.Repository(), ref.Tag())
}

func (m *manager) deleteDigestImage(repository, digestHex string) error {
	meta, err := readMetadata(m.paths, repository, digestHex)
	if err != nil {
		return err
	}
	digestTags, err := tagsForDigest(m.paths, repository, digestHex)
	if err != nil {
		return err
	}
	if err := deleteTags(m.paths, repository, digestTags); err != nil {
		return err
	}
	tagsToClean := append([]string(nil), digestTags...)
	if meta.RequestedTag != "" {
		tagsToClean = append(tagsToClean, meta.RequestedTag)
	}
	for _, claim := range meta.TagClaims {
		if claim.Repository == repository && claim.Tag != "" {
			tagsToClean = append(tagsToClean, claim.Tag)
		}
	}
	for _, tag := range tagsToClean {
		m.forgetTagState(repository, tag)
	}
	if err := m.cancelPendingTags(repository, tagsToClean); err != nil {
		return fmt.Errorf("cancel pending image tags: %w", err)
	}
	if err := removeDigestIfUnreferenced(m.paths, repository, digestHex, false); err != nil {
		return err
	}
	m.clearRequestedDigest(digestHex)
	m.refreshDiskUsageTotals()
	return nil
}

func (m *manager) deleteTaggedImage(repository, tag string) error {
	digestHex, resolveErr := resolveTag(m.paths, repository, tag)
	if resolveErr != nil && !errors.Is(resolveErr, ErrNotFound) && !os.IsNotExist(resolveErr) {
		return resolveErr
	}

	if resolveErr == nil {
		if err := deleteTag(m.paths, repository, tag); err != nil {
			return err
		}
	}
	m.forgetTagState(repository, tag)
	if err := m.cancelPendingTag(repository, tag); err != nil {
		return fmt.Errorf("cancel pending image tag: %w", err)
	}

	if resolveErr != nil {
		return resolveErr
	}

	count, err := countTagsForDigest(m.paths, repository, digestHex)
	if err != nil {
		return fmt.Errorf("count tags for digest %s: %w", digestHex, err)
	}
	if count > 0 {
		return nil
	}
	if err := removeDigestIfUnreferenced(m.paths, repository, digestHex, true); err != nil {
		return fmt.Errorf("delete orphaned digest %s: %w", digestHex, err)
	}
	m.refreshDiskUsageTotals()
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

// TotalOCICacheBytes returns the total size of the OCI and materialized layer caches.
func (m *manager) TotalOCICacheBytes(ctx context.Context) (int64, error) {
	_, ociCacheBytes, err := m.getDiskUsageTotals()
	if err != nil {
		return 0, err
	}
	return ociCacheBytes, nil
}

// WaitForReady blocks until the image reaches a terminal state (ready or failed)
// or the context is cancelled.
func (m *manager) WaitForReady(ctx context.Context, name string) error {
	ref, err := ParseNormalizedRef(name)
	if err != nil {
		return fmt.Errorf("parse image name: %w", err)
	}
	img, err := m.waitForImage(ctx, name, ref)
	if err != nil {
		return err
	}
	if terminal, err := terminalImageError(img); terminal {
		return err
	}

	digestHex := strings.TrimPrefix(img.Digest, "sha256:")

	// Subscribe BEFORE re-checking to avoid TOCTOU race
	ch := make(chan StatusEvent, 1)
	m.subscribeToReady(digestHex, ch)
	defer m.unsubscribeFromReady(digestHex, ch)

	// Re-check after subscribing to close the race window
	img, err = m.GetImage(ctx, ref.Repository()+"@"+img.Digest)
	if err == nil {
		if terminal, terminalErr := terminalImageError(img); terminal {
			return terminalErr
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

func (m *manager) waitForImage(ctx context.Context, name string, ref *NormalizedRef) (*Image, error) {
	const maxWaitForExist = 30 * time.Second
	const pollInterval = 100 * time.Millisecond
	var lastErr error
	deadline := time.Now().Add(maxWaitForExist)
	for {
		var img *Image
		if !ref.IsDigest() {
			img = m.requestedTagImage(ref)
		}
		if img == nil {
			img, lastErr = m.GetImage(ctx, name)
		}
		if img != nil {
			return img, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("get image: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func terminalImageError(img *Image) (bool, error) {
	switch img.Status {
	case StatusReady:
		return true, nil
	case StatusFailed:
		return true, conversionFailedErr(img.Error, nil)
	default:
		return false, nil
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
