package images

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// TagImage creates a ready-image tag without pulling or converting content.
// Cross-repository tags promote legacy content into the shared layout. A
// failed call leaves no side effects: the target tag's generation is only
// bumped after the new tag is on disk, so pending pulls that claimed the
// target tag keep their claim. When the target previously pointed at
// different content, that digest is collected after the new tag is live;
// cleanup failures are logged and do not fail the call, since the tag is
// already installed at that point.
//
// Promotion deliberately runs before the symlink install, so a symlink
// failure after a cross-repo promotion leaves the content promoted with no
// target tag. That state is gc-consistent (unreferenced content is
// collected) and retry is idempotent (promoteImageToContent short-circuits
// on ready content), which beats rolling back a completed promotion.
func (m *manager) TagImage(ctx context.Context, source, target string) (*Image, error) {
	sourceRef, targetRef, err := parseTagReferences(source, target)
	if err != nil {
		return nil, err
	}

	m.createMu.Lock()
	defer m.createMu.Unlock()

	digestHex, meta, err := m.readyTagImage(sourceRef)
	if err != nil {
		return nil, err
	}

	// A dangling or malformed target symlink is treated like a missing tag so
	// the retag self-heals; createTagSymlink replaces the link either way.
	previousDigest, err := resolveTag(m.paths, targetRef.Repository(), targetRef.Tag())
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, errInvalidSymlinkTarget) {
		return nil, fmt.Errorf("resolve existing target tag: %w", err)
	}
	if sourceRef.Repository() != targetRef.Repository() {
		if err := promoteImageToContent(m.paths, sourceRef.Repository(), digestHex, meta); err != nil {
			return nil, fmt.Errorf("promote image to content: %w", err)
		}
	}
	if err := createTagSymlink(m.paths, targetRef.Repository(), targetRef.Tag(), digestHex); err != nil {
		return nil, fmt.Errorf("create image tag: %w", err)
	}
	setReferenceTags(meta, targetRef.String(), nil)
	if err := writeMetadata(m.paths, targetRef.Repository(), digestHex, meta); err != nil {
		return nil, fmt.Errorf("write tagged image metadata: %w", err)
	}
	// Unlike updateExistingReference, which bumps the generation before
	// installing the symlink, the bump happens after the install here so a
	// failed tag call cannot invalidate a pending pull's claim on the target
	// tag (pinned by TestTagImageFailureLeavesNoSideEffects).
	m.nextTagGeneration(targetRef.Repository(), targetRef.Tag())
	m.cleanupReplacedTag(targetRef, previousDigest, digestHex)

	return meta.toImageFor(targetRef.String()), nil
}

func (m *manager) cleanupReplacedTag(ref *NormalizedRef, previousDigest, digestHex string) {
	if previousDigest == "" || previousDigest == digestHex {
		return
	}
	// Sibling tags in this repository may still reference the previous
	// digest; only collect when this was the last reference.
	count, err := countTagsForDigest(m.paths, ref.Repository(), previousDigest)
	if err != nil {
		slog.Warn("failed to count tags for replaced image", "repository", ref.Repository(), "digest", previousDigest, "error", err)
		return
	}
	if count > 0 {
		return
	}
	if err := removeDigestIfUnreferenced(m.paths, ref.Repository(), previousDigest, true); err != nil {
		slog.Warn("failed to collect replaced image content", "repository", ref.Repository(), "digest", previousDigest, "error", err)
	}
	m.refreshDiskUsageTotals()
}

func parseTagReferences(source, target string) (*NormalizedRef, *NormalizedRef, error) {
	sourceRef, err := ParseNormalizedRef(source)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: invalid source reference: %s", ErrInvalidName, err)
	}
	targetRef, err := ParseNormalizedRef(target)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: invalid target reference: %s", ErrInvalidName, err)
	}
	if targetRef.IsDigest() {
		return nil, nil, fmt.Errorf("%w: target must be a tag reference, not a digest", ErrInvalidName)
	}
	return sourceRef, targetRef, nil
}

func (m *manager) readyTagImage(ref *NormalizedRef) (string, *imageMetadata, error) {
	digestHex, meta, err := resolveRefMetadata(m.paths, ref)
	if err != nil {
		return "", nil, err
	}
	if meta.Status != StatusReady {
		return "", nil, fmt.Errorf("%w: %s", ErrImageNotReady, meta.Status)
	}
	return digestHex, meta, nil
}
