package imageretention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/paths"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
)

const sweepInterval = time.Minute

type imageState struct {
	Repository  string    `json:"repository"`
	Digest      string    `json:"digest"`
	UnusedSince time.Time `json:"unused_since"`
}

type digestRef struct {
	Repository string
	Digest     string
	DigestHex  string
}

func (r digestRef) key() string {
	return r.Repository + "@" + r.Digest
}

func (r digestRef) digestRef() string {
	return r.Repository + "@" + r.Digest
}

// Controller tracks unused cached images and deletes them after a retention window.
type Controller struct {
	paths        *paths.Paths
	imageManager images.Manager
	unusedFor    time.Duration
	logger       *slog.Logger
	now          func() time.Time
	mu           sync.Mutex
}

// NewController creates a new image retention controller.
func NewController(p *paths.Paths, imageManager images.Manager, unusedFor time.Duration, logger *slog.Logger) *Controller {
	if logger == nil {
		logger = slog.Default()
	}
	return &Controller{
		paths:        p,
		imageManager: imageManager,
		unusedFor:    unusedFor,
		logger:       logger.With("component", "image_retention"),
		now:          time.Now,
	}
}

// Run executes one sweep immediately, then continues sweeping every minute until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	c.logger.InfoContext(ctx, "image auto-delete scheduler started", "unused_for", c.unusedFor, "interval", sweepInterval)
	if err := c.Sweep(ctx); err != nil {
		c.logger.ErrorContext(ctx, "image auto-delete sweep failed", "error", err)
	}

	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := c.Sweep(ctx); err != nil {
				c.logger.ErrorContext(ctx, "image auto-delete sweep failed", "error", err)
			}
		}
	}
}

// Sweep performs one reconciliation pass of image retention state.
func (c *Controller) Sweep(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	stats, err := c.sweep(ctx)
	if err != nil {
		return err
	}

	c.logger.InfoContext(ctx, "image auto-delete sweep completed",
		"ready_images", stats.readyImages,
		"protected_images", stats.protectedImages,
		"marked_unused", stats.markedUnused,
		"cleared_states", stats.clearedStates,
		"deleted_images", stats.deletedImages,
		"pruned_states", stats.prunedStates,
		"stale_references", stats.staleReferences,
	)
	return nil
}

// MarkUsed clears retention state for a digest that is about to be referenced by a new instance.
func (c *Controller) MarkUsed(ctx context.Context, imageName, digest string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	ref, err := c.resolveDigestRef(ctx, imageName, digest)
	if err != nil {
		return err
	}
	removed, err := c.deleteStateIfExists(ref)
	if err != nil {
		return err
	}
	if removed {
		c.logger.DebugContext(ctx, "cleared image auto-delete state", "image", imageName, "digest", ref.Digest)
	}
	return nil
}

type sweepStats struct {
	readyImages     int
	protectedImages int
	markedUnused    int
	clearedStates   int
	deletedImages   int
	prunedStates    int
	staleReferences int
}

func (c *Controller) sweep(ctx context.Context) (sweepStats, error) {
	var stats sweepStats

	allImages, err := c.imageManager.ListImages(ctx)
	if err != nil {
		return stats, fmt.Errorf("list images: %w", err)
	}

	ready := make(map[string]digestRef)
	allLocal := make(map[string]struct{})
	nonReady := make(map[string]digestRef)
	for _, img := range allImages {
		ref, err := digestRefFromImage(img)
		if err != nil {
			c.logger.WarnContext(ctx, "skipping image with invalid retention metadata", "image", img.Name, "digest", img.Digest, "error", err)
			continue
		}
		allLocal[ref.key()] = struct{}{}
		if img.Status == images.StatusReady {
			ready[ref.key()] = ref
			stats.readyImages++
			continue
		}
		nonReady[ref.key()] = ref
	}

	states, err := c.listStates()
	if err != nil {
		return stats, err
	}

	protected, staleRefs, err := c.collectProtectedDigests(ctx)
	if err != nil {
		return stats, err
	}
	stats.protectedImages = len(protected)
	stats.staleReferences = staleRefs

	for _, ref := range protected {
		removed, err := c.deleteStateIfExists(ref)
		if err != nil {
			return stats, err
		}
		if removed {
			stats.clearedStates++
		}
	}

	for _, ref := range nonReady {
		removed, err := c.deleteStateIfExists(ref)
		if err != nil {
			return stats, err
		}
		if removed {
			stats.clearedStates++
		}
	}

	for key, state := range states {
		if _, ok := allLocal[key]; ok {
			continue
		}
		removed, err := c.deleteStateIfExists(digestRef{
			Repository: state.Repository,
			Digest:     state.Digest,
			DigestHex:  strings.TrimPrefix(state.Digest, "sha256:"),
		})
		if err != nil {
			return stats, err
		}
		if removed {
			stats.prunedStates++
		}
	}

	now := c.now().UTC()
	for key, ref := range ready {
		if _, ok := protected[key]; ok {
			continue
		}

		state, ok := states[key]
		if !ok {
			if err := c.writeState(imageState{
				Repository:  ref.Repository,
				Digest:      ref.Digest,
				UnusedSince: now,
			}); err != nil {
				return stats, err
			}
			stats.markedUnused++
			continue
		}

		if state.UnusedSince.IsZero() {
			state.UnusedSince = now
			if err := c.writeState(state); err != nil {
				return stats, err
			}
			stats.markedUnused++
			continue
		}

		if state.UnusedSince.UTC().Add(c.unusedFor).After(now) {
			continue
		}

		if err := c.imageManager.DeleteImage(ctx, ref.digestRef()); err != nil {
			if errors.Is(err, images.ErrNotFound) {
				removed, removeErr := c.deleteStateIfExists(ref)
				if removeErr != nil {
					return stats, removeErr
				}
				if removed {
					stats.prunedStates++
				}
				continue
			}
			return stats, fmt.Errorf("delete image %s: %w", ref.digestRef(), err)
		}

		c.logger.InfoContext(ctx, "deleted unused cached image", "image", ref.digestRef(), "unused_since", state.UnusedSince, "unused_for", c.unusedFor)
		removed, err := c.deleteStateIfExists(ref)
		if err != nil {
			return stats, err
		}
		if removed {
			stats.deletedImages++
		}
	}

	return stats, nil
}

func (c *Controller) collectProtectedDigests(ctx context.Context) (map[string]digestRef, int, error) {
	imageRefs, err := c.collectReferencedImageNames(ctx)
	if err != nil {
		return nil, 0, err
	}

	protected := make(map[string]digestRef, len(imageRefs))
	staleRefs := 0
	for imageName := range imageRefs {
		img, err := c.imageManager.GetImage(ctx, imageName)
		if err != nil {
			staleRefs++
			c.logger.WarnContext(ctx, "skipping stale image reference during retention sweep", "image", imageName, "error", err)
			continue
		}
		ref, err := digestRefFromImage(*img)
		if err != nil {
			staleRefs++
			c.logger.WarnContext(ctx, "skipping unresolvable retained image", "image", imageName, "digest", img.Digest, "error", err)
			continue
		}
		protected[ref.key()] = ref
	}
	return protected, staleRefs, nil
}

func (c *Controller) collectReferencedImageNames(ctx context.Context) (map[string]struct{}, error) {
	_ = ctx

	refs := make(map[string]struct{})
	guestDir := c.paths.GuestsDir()
	if entries, err := os.ReadDir(guestDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			metaPath := c.paths.InstanceMetadata(entry.Name())
			content, err := os.ReadFile(metaPath)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return nil, fmt.Errorf("read instance metadata %s: %w", metaPath, err)
			}

			var stored instances.StoredMetadata
			if err := json.Unmarshal(content, &stored); err != nil {
				c.logger.Warn("skipping invalid instance metadata during retention sweep", "path", metaPath, "error", err)
				continue
			}
			if stored.Image != "" {
				refs[stored.Image] = struct{}{}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read guests directory: %w", err)
	}

	store := snapshotstore.NewStore(c.paths)
	records, err := store.ListRecords()
	if err != nil {
		return nil, fmt.Errorf("list snapshot records: %w", err)
	}
	for _, record := range records {
		if len(record.StoredMetadata) == 0 {
			continue
		}
		var stored instances.StoredMetadata
		if err := json.Unmarshal(record.StoredMetadata, &stored); err != nil {
			c.logger.Warn("skipping invalid snapshot stored metadata during retention sweep", "snapshot_id", record.Snapshot.Id, "error", err)
			continue
		}
		if stored.Image != "" {
			refs[stored.Image] = struct{}{}
		}
	}

	return refs, nil
}

func (c *Controller) resolveDigestRef(ctx context.Context, imageName, digest string) (digestRef, error) {
	if strings.TrimSpace(digest) == "" {
		img, err := c.imageManager.GetImage(ctx, imageName)
		if err != nil {
			return digestRef{}, err
		}
		return digestRefFromImage(*img)
	}

	ref, err := images.ParseNormalizedRef(imageName)
	if err != nil {
		return digestRef{}, fmt.Errorf("parse image name: %w", err)
	}
	return digestRef{
		Repository: ref.Repository(),
		Digest:     normalizeDigest(digest),
		DigestHex:  strings.TrimPrefix(normalizeDigest(digest), "sha256:"),
	}, nil
}

func digestRefFromImage(img images.Image) (digestRef, error) {
	if strings.TrimSpace(img.Digest) == "" {
		return digestRef{}, fmt.Errorf("missing digest")
	}
	ref, err := images.ParseNormalizedRef(img.Name)
	if err != nil {
		return digestRef{}, err
	}
	digest := normalizeDigest(img.Digest)
	return digestRef{
		Repository: ref.Repository(),
		Digest:     digest,
		DigestHex:  strings.TrimPrefix(digest, "sha256:"),
	}, nil
}

func normalizeDigest(digest string) string {
	digest = strings.TrimSpace(digest)
	if strings.HasPrefix(digest, "sha256:") {
		return digest
	}
	return "sha256:" + digest
}

func (c *Controller) listStates() (map[string]imageState, error) {
	root := c.paths.SystemImageRetentionDir()
	states := make(map[string]imageState)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if d.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read image retention state %s: %w", path, err)
		}

		var state imageState
		if err := json.Unmarshal(content, &state); err != nil {
			return fmt.Errorf("unmarshal image retention state %s: %w", path, err)
		}

		key := state.Repository + "@" + state.Digest
		states[key] = state
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("list image retention state: %w", err)
	}

	return states, nil
}

func (c *Controller) writeState(state imageState) error {
	path := c.paths.ImageRetentionState(state.Repository, strings.TrimPrefix(state.Digest, "sha256:"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create image retention directory: %w", err)
	}

	content, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal image retention state: %w", err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, content, 0o644); err != nil {
		return fmt.Errorf("write image retention temp state: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename image retention state: %w", err)
	}
	return nil
}

func (c *Controller) deleteStateIfExists(ref digestRef) (bool, error) {
	path := c.paths.ImageRetentionState(ref.Repository, ref.DigestHex)
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("remove image retention state %s: %w", path, err)
	}
	c.removeEmptyParents(filepath.Dir(path), c.paths.SystemImageRetentionDir())
	return true, nil
}

func (c *Controller) removeEmptyParents(path string, root string) {
	root = filepath.Clean(root)
	for {
		cleaned := filepath.Clean(path)
		if cleaned == root || cleaned == "." || cleaned == string(filepath.Separator) {
			return
		}
		err := os.Remove(cleaned)
		if err != nil {
			return
		}
		path = filepath.Dir(cleaned)
	}
}
