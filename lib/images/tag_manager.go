package images

import (
	"errors"
	"log/slog"
	"strings"
	"time"
)

func tagKey(repository, tag string) string {
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
	key := tagKey(repository, tag)
	m.tagGenerations[key]++
	return m.tagGenerations[key]
}

func (m *manager) revertTagGeneration(repository, tag string) {
	key := tagKey(repository, tag)
	if m.tagGenerations[key] <= 1 {
		delete(m.tagGenerations, key)
		return
	}
	m.tagGenerations[key]--
}

func (m *manager) releaseTagGeneration(repository, tag string, generation uint64) {
	if m.tagGenerations[tagKey(repository, tag)] == generation {
		m.revertTagGeneration(repository, tag)
	}
}

func (m *manager) forgetTagState(repository, tag string) {
	delete(m.tagGenerations, tagKey(repository, tag))
	m.clearRequestedTag(repository, tag, "")
}

func (m *manager) restoreTagState(metas []*imageMetadata) {
	m.createMu.Lock()
	defer m.createMu.Unlock()
	m.ensureTagState()
	for _, meta := range metas {
		if !isPendingImageStatus(meta.Status) || meta.Digest == "" {
			continue
		}
		digestHex := meta.digestHex()
		ref, err := ParseNormalizedRef(meta.Name)
		if err != nil {
			continue
		}
		if meta.RequestedTag != "" && !meta.TagClaimCanceled {
			m.restoreRequestedTag(ref.Repository(), meta.RequestedTag, digestHex, meta.TagGeneration)
		}
		for _, claim := range meta.TagClaims {
			m.restoreRequestedTag(claim.Repository, claim.Tag, digestHex, claim.TagGeneration)
		}
	}
}

func (m *manager) restoreRequestedTag(repository, tag, digestHex string, generation uint64) {
	if tag == "" {
		return
	}
	key := tagKey(repository, tag)
	if currentGen, ok := m.tagGenerations[key]; !ok || generation >= currentGen {
		m.tagGenerations[key] = generation
		m.requestedTags[key] = digestHex
	}
}

func (m *manager) claimTagForStatus(meta *imageMetadata, ref *ResolvedRef) error {
	if ref.Tag() == "" {
		return nil
	}
	if meta.Status == StatusReady {
		return m.claimReadyTag(ref.Repository(), ref.Tag(), ref.DigestHex())
	}
	if meta.RequestedTag == ref.Tag() && strings.HasPrefix(meta.Name, ref.Repository()+":") {
		wasCanceled := meta.TagClaimCanceled
		meta.TagClaimCanceled = false
		m.trackRequestedTag(ref.Repository(), ref.Tag(), ref.DigestHex())
		if wasCanceled {
			return writeMetadata(m.paths, ref.Repository(), ref.DigestHex(), meta)
		}
		return nil
	}
	for _, claim := range meta.TagClaims {
		if claim.Repository == ref.Repository() && claim.Tag == ref.Tag() {
			m.restoreRequestedTag(claim.Repository, claim.Tag, ref.DigestHex(), claim.TagGeneration)
			return nil
		}
	}
	previous, err := resolveTag(m.paths, ref.Repository(), ref.Tag())
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	generation := m.nextTagGeneration(ref.Repository(), ref.Tag())
	meta.TagClaims = append(meta.TagClaims, imageTagClaim{
		Repository:        ref.Repository(),
		Tag:               ref.Tag(),
		PreviousTagDigest: previous,
		TagGeneration:     generation,
	})
	if previous == "" {
		if err := createTagSymlink(m.paths, ref.Repository(), ref.Tag(), ref.DigestHex()); err != nil {
			m.revertTagGeneration(ref.Repository(), ref.Tag())
			meta.TagClaims = meta.TagClaims[:len(meta.TagClaims)-1]
			return err
		}
	}
	if err := writeMetadata(m.paths, ref.Repository(), ref.DigestHex(), meta); err != nil {
		if previous == "" {
			_ = deleteTag(m.paths, ref.Repository(), ref.Tag())
		}
		m.revertTagGeneration(ref.Repository(), ref.Tag())
		meta.TagClaims = meta.TagClaims[:len(meta.TagClaims)-1]
		return err
	}
	m.trackRequestedTag(ref.Repository(), ref.Tag(), ref.DigestHex())
	return nil
}

func (m *manager) claimReadyTag(repository, tag, digestHex string) error {
	m.nextTagGeneration(repository, tag)
	if err := createTagSymlink(m.paths, repository, tag, digestHex); err != nil {
		m.revertTagGeneration(repository, tag)
		return err
	}
	m.clearRequestedTag(repository, tag, "")
	return nil
}

func (m *manager) trackRequestedTag(repository, tag, digestHex string) {
	m.ensureTagState()
	m.requestedTags[tagKey(repository, tag)] = digestHex
}

func (m *manager) clearRequestedTag(repository, tag, digestHex string) {
	key := tagKey(repository, tag)
	if current, ok := m.requestedTags[key]; ok && (digestHex == "" || current == digestHex) {
		delete(m.requestedTags, key)
	}
}

func (m *manager) clearRequestedDigest(digestHex string) {
	for key, current := range m.requestedTags {
		if current == digestHex {
			delete(m.requestedTags, key)
			repo, tag, _ := strings.Cut(key, ":")
			if next := m.findPendingDigestForTag(repo, tag, digestHex); next != "" {
				m.requestedTags[key] = next
			}
		}
	}
}

func (m *manager) findPendingDigestForTag(repository, tag, excludeDigestHex string) string {
	metas, err := listAllMetadata(m.paths)
	if err != nil {
		return ""
	}
	var (
		bestDigest string
		bestTime   time.Time
		bestGen    uint64
	)
	for _, meta := range metas {
		if !isPendingImageStatus(meta.Status) || meta.digestHex() == excludeDigestHex {
			continue
		}
		if !meta.TagClaimCanceled && meta.RequestedTag == tag {
			ref, err := ParseNormalizedRef(meta.Name)
			if err == nil && ref.Repository() == repository {
				if meta.TagGeneration >= bestGen || (meta.TagGeneration == bestGen && meta.CreatedAt.After(bestTime)) {
					bestDigest, bestTime, bestGen = meta.digestHex(), meta.CreatedAt, meta.TagGeneration
				}
			}
		}
		for _, claim := range meta.TagClaims {
			if claim.Repository == repository && claim.Tag == tag {
				if claim.TagGeneration >= bestGen || (claim.TagGeneration == bestGen && meta.CreatedAt.After(bestTime)) {
					bestDigest, bestTime, bestGen = meta.digestHex(), meta.CreatedAt, claim.TagGeneration
				}
			}
		}
	}
	return bestDigest
}

func (m *manager) cancelPendingTag(repository, tag string) error {
	return m.cancelPendingTags(repository, []string{tag})
}

func (m *manager) cancelPendingTags(repository string, tags []string) error {
	if len(tags) == 0 {
		return nil
	}
	tagSet := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		tagSet[tag] = struct{}{}
	}
	metas, err := listAllMetadata(m.paths)
	if err != nil {
		return err
	}
	seenDigests := make(map[string]struct{}, len(metas))
	for _, meta := range metas {
		if !isPendingImageStatus(meta.Status) {
			continue
		}
		digestHex := meta.digestHex()
		if _, seen := seenDigests[digestHex]; seen {
			continue
		}
		seenDigests[digestHex] = struct{}{}

		canonicalMeta, err := readMetadata(m.paths, repository, digestHex)
		if err != nil {
			canonicalMeta = meta
		}

		ref, err := ParseNormalizedRef(canonicalMeta.Name)
		if canonicalMeta.Request != nil && canonicalMeta.Request.Name != "" {
			if reqRef, err := ParseNormalizedRef(canonicalMeta.Request.Name); err == nil {
				ref = reqRef
			}
		}
		if ref == nil {
			continue
		}

		changed := false
		for tag := range tagSet {
			if cancelTagClaim(canonicalMeta, ref, repository, tag) {
				changed = true
			}
		}
		if !changed {
			continue
		}
		if err := writeMetadata(m.paths, ref.Repository(), digestHex, canonicalMeta); err != nil {
			return err
		}
	}
	return nil
}

func isPendingImageStatus(status string) bool {
	return status == StatusPending || status == StatusPulling || status == StatusConverting
}

func cancelTagClaim(meta *imageMetadata, ref *NormalizedRef, repository, tag string) bool {
	changed := false
	if ref.Repository() == repository && meta.RequestedTag == tag {
		if !meta.TagClaimCanceled {
			meta.TagClaimCanceled = true
			changed = true
		}
	}
	claims := meta.TagClaims[:0]
	for _, claim := range meta.TagClaims {
		if claim.Repository == repository && claim.Tag == tag {
			changed = true
			continue
		}
		claims = append(claims, claim)
	}
	meta.TagClaims = claims
	return changed
}

func (m *manager) requestedTagImage(ref *NormalizedRef) *Image {
	m.createMu.Lock()
	digestHex, ok := m.requestedTags[tagKey(ref.Repository(), ref.Tag())]
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

func (m *manager) claimRequestedTags(ref *ResolvedRef, meta *imageMetadata) bool {
	digestHex := ref.DigestHex()
	primaryTag := meta.RequestedTag
	if primaryTag == "" {
		primaryTag = ref.Tag()
	}
	primaryKey := tagKey(ref.Repository(), primaryTag)

	claimed, primaryClaimed := false, false
	for key, requestedDigest := range m.requestedTags {
		if requestedDigest != digestHex {
			continue
		}
		repo, tag, _ := strings.Cut(key, ":")
		if err := createTagSymlink(m.paths, repo, tag, digestHex); err != nil {
			slog.Warn("failed to claim image tag", "repository", repo, "tag", tag, "digest", digestHex, "error", err)
			continue
		}
		delete(m.requestedTags, key)
		claimed = true
		if key == primaryKey {
			primaryClaimed = true
		}
	}
	if !primaryClaimed && m.claimPrimaryTag(ref, meta, digestHex) {
		claimed = true
	}
	for _, claim := range meta.TagClaims {
		claimKey := tagKey(claim.Repository, claim.Tag)
		if m.tagClaimIsCurrent(claim.Repository, claim.Tag, digestHex, meta, false) {
			if _, hasReq := m.requestedTags[claimKey]; !hasReq {
				if err := createTagSymlink(m.paths, claim.Repository, claim.Tag, digestHex); err == nil {
					claimed = true
				}
			}
		}
	}
	return claimed || (meta.RequestedTag == "" && ref.Tag() == "")
}

func (m *manager) claimPrimaryTag(ref *ResolvedRef, meta *imageMetadata, digestHex string) bool {
	if meta.TagClaimCanceled {
		return false
	}
	primaryTag := meta.RequestedTag
	if primaryTag == "" {
		primaryTag = ref.Tag()
	}
	if primaryTag == "" {
		return false
	}
	primaryKey := tagKey(ref.Repository(), primaryTag)
	if _, hasRequest := m.requestedTags[primaryKey]; hasRequest || !m.tagClaimIsCurrent(ref.Repository(), primaryTag, digestHex, meta, meta.RequestedTag == "") {
		return false
	}
	if err := createTagSymlink(m.paths, ref.Repository(), primaryTag, digestHex); err != nil {
		slog.Warn("failed to claim image tag", "repository", ref.Repository(), "tag", primaryTag, "digest", digestHex, "error", err)
		return false
	}
	return true
}

func (m *manager) tagClaimIsCurrent(repository, tag, digest string, meta *imageMetadata, allowMissing bool) bool {
	if tag == "" || m.tagGenerations[tagKey(repository, tag)] != meta.TagGeneration {
		return false
	}
	current, err := resolveTag(m.paths, repository, tag)
	if err == nil {
		return current == digest || current == meta.PreviousTagDigest
	}
	return allowMissing && errors.Is(err, ErrNotFound)
}
