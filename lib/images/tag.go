package images

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/kernel/hypeman/lib/paths"
)

// TagImage creates a ready-image tag without pulling or converting content.
// Cross-repository tags promote content into the shared layout.
func (m *manager) TagImage(ctx context.Context, source, target string) (*Image, error) {
	sourceRef, targetRef, err := parseTagReferences(source, target)
	if err != nil {
		return nil, err
	}

	m.createMu.Lock()
	defer m.createMu.Unlock()

	previousDigest, err := existingTagDigest(m.paths, targetRef)
	if err != nil {
		return nil, err
	}
	digestHex, meta, err := m.readyTagImage(sourceRef)
	if err != nil {
		return nil, err
	}
	if err := m.cancelPendingTag(targetRef.Repository(), targetRef.Tag()); err != nil {
		return nil, fmt.Errorf("cancel pending image tag: %w", err)
	}
	if err := m.installTag(sourceRef, targetRef, digestHex, meta); err != nil {
		return nil, err
	}
	m.nextTagGeneration(targetRef.Repository(), targetRef.Tag())
	m.clearRequestedTag(targetRef.Repository(), targetRef.Tag(), "")
	m.cleanupReplacedTag(targetRef, previousDigest, digestHex)

	return meta.toImageFor(targetRef.String()), nil
}

func existingTagDigest(p *paths.Paths, ref *NormalizedRef) (string, error) {
	digest, err := resolveTag(p, ref.Repository(), ref.Tag())
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, errInvalidSymlinkTarget) {
		return digest, nil
	}
	return "", fmt.Errorf("resolve existing target tag: %w", err)
}

func (m *manager) installTag(source, target *NormalizedRef, digest string, meta *imageMetadata) error {
	if source.Repository() != target.Repository() {
		if err := promoteImageToContent(m.paths, source.Repository(), digest, meta); err != nil {
			return fmt.Errorf("promote image to content: %w", err)
		}
	}
	installed, err := installTagSymlink(m.paths, target.Repository(), target.Tag(), digest)
	if err != nil {
		return fmt.Errorf("create image tag: %w", err)
	}
	if err := writeMetadata(m.paths, target.Repository(), digest, meta); err != nil {
		if restoreErr := restoreTagSymlink(&installed); restoreErr != nil {
			return errors.Join(
				fmt.Errorf("write tagged image metadata: %w", err),
				fmt.Errorf("restore image tag: %w", restoreErr),
			)
		}
		return fmt.Errorf("write tagged image metadata: %w", err)
	}
	return nil
}

func (m *manager) cleanupUnclaimedImage(ref *ResolvedRef) {
	if err := removeDigestIfUnreferenced(m.paths, ref.Repository(), ref.DigestHex(), true); err != nil {
		slog.Warn("failed to collect stale image", "repository", ref.Repository(), "digest", ref.DigestHex(), "error", err)
	}
}

func (m *manager) cleanupReplacedTag(ref *NormalizedRef, previousDigest, digestHex string) {
	if previousDigest == "" || previousDigest == digestHex {
		return
	}
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
	m.reconcileLayerStoreLocked()
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
