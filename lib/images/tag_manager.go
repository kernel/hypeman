package images

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/kernel/hypeman/lib/paths"
)

func tagGenerationKey(repository, tag string) string {
	return repository + ":" + tag
}

func (m *manager) ensureTagState() {
	if m.tagGenerations == nil {
		m.tagGenerations = make(map[string]uint64)
	}
	if m.requestedTags == nil {
		m.requestedTags = make(map[string]string)
	}
}

func (m *manager) nextTagGeneration(repository, tag string) uint64 {
	m.ensureTagState()
	key := tagGenerationKey(repository, tag)
	m.tagGenerations[key]++
	return m.tagGenerations[key]
}

func (m *manager) revertTagGeneration(repository, tag string) {
	key := tagGenerationKey(repository, tag)
	if m.tagGenerations[key] <= 1 {
		delete(m.tagGenerations, key)
		return
	}
	m.tagGenerations[key]--
}

func (m *manager) releaseTagGeneration(repository, tag string, generation uint64) {
	if m.tagGenerations[tagGenerationKey(repository, tag)] == generation {
		m.revertTagGeneration(repository, tag)
	}
}

func (m *manager) pruneTagGenerations() {
	for key := range m.tagGenerations {
		colon := strings.LastIndexByte(key, ':')
		if colon < 0 {
			continue
		}
		if _, err := resolveTag(m.paths, key[:colon], key[colon+1:]); err != nil {
			delete(m.tagGenerations, key)
		}
	}
}

func (m *manager) restoreTagState(metas []*imageMetadata) {
	m.ensureTagState()
	for _, meta := range metas {
		if meta.Status != StatusPending && meta.Status != StatusPulling && meta.Status != StatusConverting {
			continue
		}
		if meta.RequestedTag == "" || meta.Digest == "" {
			continue
		}
		ref, err := ParseNormalizedRef(meta.Name)
		if err != nil {
			continue
		}
		key := tagGenerationKey(ref.Repository(), meta.RequestedTag)
		if meta.TagGeneration > m.tagGenerations[key] {
			m.tagGenerations[key] = meta.TagGeneration
		}
		m.requestedTags[key] = strings.TrimPrefix(meta.Digest, "sha256:")
	}
}

func (m *manager) claimTagForStatus(meta *imageMetadata, ref *ResolvedRef) error {
	if ref.Tag() == "" {
		return nil
	}
	if meta.Status == StatusReady {
		return m.claimReadyTag(ref.Repository(), ref.Tag(), ref.DigestHex())
	}
	// Keep the existing tag visible until the build succeeds, but remember this
	// digest as the newest request for the tag. Finalization will claim it along
	// with every other tag waiting for the same digest.
	m.nextTagGeneration(ref.Repository(), ref.Tag())
	m.trackRequestedTag(ref.Repository(), ref.Tag(), ref.DigestHex())
	return nil
}

func (m *manager) claimReadyTag(repository, tag, digestHex string) error {
	m.nextTagGeneration(repository, tag)
	if err := createTagSymlink(m.paths, repository, tag, digestHex); err != nil {
		m.revertTagGeneration(repository, tag)
		return err
	}
	m.clearRequestedTag(repository, tag, digestHex)
	return nil
}

func (m *manager) trackRequestedTag(repository, tag, digestHex string) {
	m.ensureTagState()
	m.requestedTags[tagGenerationKey(repository, tag)] = digestHex
}

func (m *manager) clearRequestedTag(repository, tag, digestHex string) {
	key := tagGenerationKey(repository, tag)
	if current, ok := m.requestedTags[key]; ok && (digestHex == "" || current == digestHex) {
		delete(m.requestedTags, key)
	}
}

func (m *manager) clearRequestedDigest(digestHex string) {
	for key, current := range m.requestedTags {
		if current == digestHex {
			delete(m.requestedTags, key)
		}
	}
}

func (m *manager) requestedTagImage(ref *NormalizedRef) *Image {
	m.createMu.Lock()
	digestHex, ok := m.requestedTags[tagGenerationKey(ref.Repository(), ref.Tag())]
	m.createMu.Unlock()
	if !ok {
		return nil
	}
	meta, err := readMetadata(m.paths, ref.Repository(), digestHex)
	if err != nil {
		return nil
	}
	return meta.toImageFor(ref.String())
}

func (m *manager) claimRequestedTag(ref *ResolvedRef, meta *imageMetadata) bool {
	tag := meta.RequestedTag
	allowMissing := tag == ""
	if allowMissing {
		tag = ref.Tag()
	}
	if !m.tagClaimIsCurrent(ref.Repository(), tag, ref.DigestHex(), meta, allowMissing) {
		m.clearRequestedTag(ref.Repository(), tag, ref.DigestHex())
		return false
	}
	if err := createTagSymlink(m.paths, ref.Repository(), tag, ref.DigestHex()); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to create tag symlink: %v\n", err)
		return false
	}
	m.clearRequestedTag(ref.Repository(), tag, ref.DigestHex())
	return true
}

func (m *manager) claimRequestedTags(ref *ResolvedRef, meta *imageMetadata) bool {
	digestHex := ref.DigestHex()
	claimed := false
	for key, requestedDigest := range m.requestedTags {
		if requestedDigest != digestHex {
			continue
		}
		separator := strings.LastIndexByte(key, ':')
		if separator <= 0 || separator == len(key)-1 {
			continue
		}
		repository, tag := key[:separator], key[separator+1:]
		if err := createTagSymlink(m.paths, repository, tag, digestHex); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to create tag symlink: %v\n", err)
			continue
		}
		delete(m.requestedTags, key)
		claimed = true
	}
	if claimed {
		return true
	}
	// Digest-only images have no tag claim but must remain addressable by their
	// digest after finalization.
	return meta.RequestedTag == "" && ref.Tag() == ""
}

func (m *manager) tagClaimIsCurrent(repository, tag, digest string, meta *imageMetadata, allowMissing bool) bool {
	if tag == "" || m.tagGenerations[tagGenerationKey(repository, tag)] != meta.TagGeneration {
		return false
	}
	current, err := resolveTag(m.paths, repository, tag)
	if err == nil {
		return current == digest || current == meta.PreviousTagDigest
	}
	return allowMissing && errors.Is(err, ErrNotFound)
}

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
