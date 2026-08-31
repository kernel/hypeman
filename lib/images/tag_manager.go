package images

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

func tagGenerationKey(repository, tag string) string {
	return repository + ":" + tag
}

// nextTagGeneration bumps the mutation generation for a tag and returns the
// new value. Pulls record the value with their metadata and only repoint the
// tag on completion when no later operation mutated it.
func (m *manager) nextTagGeneration(repository, tag string) uint64 {
	if m.tagGenerations == nil {
		m.tagGenerations = make(map[string]uint64)
	}
	key := tagGenerationKey(repository, tag)
	m.tagGenerations[key]++
	return m.tagGenerations[key]
}

// revertTagGeneration undoes a nextTagGeneration bump for an operation that
// failed before mutating the tag, so an in-flight pull of the tag is not
// permanently blocked from repointing it.
func (m *manager) revertTagGeneration(repository, tag string) {
	key := tagGenerationKey(repository, tag)
	if m.tagGenerations[key] <= 1 {
		delete(m.tagGenerations, key)
		return
	}
	m.tagGenerations[key]--
}

// releaseTagGeneration drops a failed pull's claim on its tag so an older
// in-flight pull of the same tag can still repoint it.
func (m *manager) releaseTagGeneration(repository, tag string, generation uint64) {
	if m.tagGenerations[tagGenerationKey(repository, tag)] == generation {
		m.revertTagGeneration(repository, tag)
	}
}

// pruneTagGenerations drops entries for tags that no longer resolve, keeping
// the map bounded over the process lifetime. Deleting a tag still suppresses
// an in-flight pull's repoint: the pull's recorded generation can no longer
// match the missing entry.
func (m *manager) pruneTagGenerations() {
	for key := range m.tagGenerations {
		// tagGenerationKey is repo+":"+tag and tags never contain ":", but
		// repositories may (host:port), so split on the last colon.
		colon := strings.LastIndexByte(key, ':')
		if colon < 0 {
			continue
		}
		if _, err := resolveTag(m.paths, key[:colon], key[colon+1:]); err != nil {
			delete(m.tagGenerations, key)
		}
	}
}

// restoreTagState re-seeds the tag indexes from recovered metadata. metas must
// be sorted oldest first so the newest requested pull wins.
func (m *manager) restoreTagState(metas []*imageMetadata) {
	if m.tagGenerations == nil {
		m.tagGenerations = make(map[string]uint64)
	}
	if m.requestedTags == nil {
		m.requestedTags = make(map[string]string)
	}
	for _, meta := range metas {
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

// claimTagForStatus repoints ref's tag at the digest when the image is ready,
// or records a pending tag when the build is still in flight. It is a no-op
// for digest-only references.
func (m *manager) claimTagForStatus(meta *imageMetadata, ref *ResolvedRef) error {
	if ref.Tag() == "" {
		return nil
	}
	if meta.Status == StatusReady {
		return m.claimReadyTag(ref.Repository(), ref.Tag(), ref.DigestHex())
	}
	return ensurePendingTag(m.paths, ref.Repository(), ref.Tag(), ref.DigestHex())
}

// claimReadyTag repoints an existing tag at a ready digest, last pull wins.
func (m *manager) claimReadyTag(repository, tag, digestHex string) error {
	m.nextTagGeneration(repository, tag)
	if err := createTagSymlink(m.paths, repository, tag, digestHex); err != nil {
		m.revertTagGeneration(repository, tag)
		return err
	}
	return nil
}

// trackRequestedTag records the digest of the newest pull requested for a tag
// so readiness waits can find it without walking the metadata tree.
func (m *manager) trackRequestedTag(repository, tag, digestHex string) {
	if m.requestedTags == nil {
		m.requestedTags = make(map[string]string)
	}
	m.requestedTags[tagGenerationKey(repository, tag)] = digestHex
}

// requestedTagImage returns the newest image requested for a tag, so a
// readiness wait tracks the latest pull rather than the digest the tag
// currently points at.
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

// claimRequestedTag repoints the pull's requested tag at the finished digest.
// The tag is only repointed when its generation still matches the pull's
// recorded claim, so a later mutation of the tag wins over the in-flight
// pull. A recovered build whose metadata predates requested-tag tracking has
// no recorded tag: fall back to the reference's own tag and recreate a
// missing symlink, matching the pre-tracking behavior. Otherwise an
// already-missing tag stays missing so a concurrent delete wins.
func (m *manager) claimRequestedTag(ref *ResolvedRef, meta *imageMetadata) bool {
	requestedTag := meta.RequestedTag
	allowMissing := requestedTag == ""
	if requestedTag == "" {
		requestedTag = ref.Tag()
	}
	if requestedTag == "" || m.tagGenerations[tagGenerationKey(ref.Repository(), requestedTag)] != meta.TagGeneration {
		return false
	}
	current, err := resolveTag(m.paths, ref.Repository(), requestedTag)
	if err != nil {
		if !allowMissing || !errors.Is(err, ErrNotFound) {
			return false
		}
	} else if current != ref.DigestHex() && current != meta.PreviousTagDigest {
		return false
	}
	if err := createTagSymlink(m.paths, ref.Repository(), requestedTag, ref.DigestHex()); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to create tag symlink: %v\n", err)
		return false
	}
	return true
}

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

	// A dangling or malformed target symlink is treated like a missing tag so
	// the retag self-heals; createTagSymlink replaces the link either way.
	previousDigest, err := resolveTag(m.paths, targetRef.Repository(), targetRef.Tag())
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, errInvalidSymlinkTarget) {
		return nil, fmt.Errorf("resolve existing target tag: %w", err)
	}

	digestHex, meta, err := m.readyTagImage(sourceRef)
	if err != nil {
		return nil, err
	}
	if sourceRef.Repository() != targetRef.Repository() {
		if err := promoteImageToContent(m.paths, sourceRef.Repository(), digestHex, meta); err != nil {
			return nil, fmt.Errorf("promote image to content: %w", err)
		}
	}
	if err := createTagSymlink(m.paths, targetRef.Repository(), targetRef.Tag(), digestHex); err != nil {
		return nil, fmt.Errorf("create image tag: %w", err)
	}
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

func (m *manager) cleanupUnclaimedImage(ref *ResolvedRef) {
	if err := removeDigestIfUnreferenced(m.paths, ref.Repository(), ref.DigestHex(), true); err != nil {
		slog.Warn("failed to collect stale image", "repository", ref.Repository(), "digest", ref.DigestHex(), "error", err)
	}
}

func (m *manager) cleanupReplacedTag(ref *NormalizedRef, previousDigest, digestHex string) {
	if previousDigest != "" && previousDigest != digestHex {
		// Sibling tags in this repository may still reference the previous
		// digest; only collect when this was the last reference.
		count, err := countTagsForDigest(m.paths, ref.Repository(), previousDigest)
		if err != nil {
			slog.Warn("failed to count tags for replaced image", "repository", ref.Repository(), "digest", previousDigest, "error", err)
		} else if count == 0 {
			if err := removeDigestIfUnreferenced(m.paths, ref.Repository(), previousDigest, true); err != nil {
				slog.Warn("failed to collect replaced image content", "repository", ref.Repository(), "digest", previousDigest, "error", err)
			}
			m.refreshDiskUsageTotals()
		}
	}
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
