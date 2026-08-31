package images

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/tags"
	"github.com/stretchr/testify/require"
)

func seedContent(t *testing.T, p *paths.Paths, repository, tag, digestHex string) {
	t.Helper()
	seedImage(t, p, repository, tag, digestHex, true)
}

func seedLegacy(t *testing.T, p *paths.Paths, repository, tag, digestHex string) {
	t.Helper()
	seedImage(t, p, repository, tag, digestHex, false)
}

func seedImage(t *testing.T, p *paths.Paths, repository, tag, digestHex string, content bool) {
	t.Helper()
	dir := p.ImageDigestDir(repository, digestHex)
	disk := p.ImageDigestPath(repository, digestHex)
	metadata := p.ImageMetadata(repository, digestHex)
	linkPath := p.ImageTagSymlink(repository, tag)
	target := digestHex
	if content {
		dir = p.ImageContentDir(digestHex)
		disk = p.ImageContentPath(digestHex)
		metadata = p.ImageContentMetadata(digestHex)
		linkPath = p.ImageRepositoryTagSymlink(repository, tag)
		rel, err := filepath.Rel(filepath.Dir(linkPath), p.ImageContentDir(digestHex))
		require.NoError(t, err)
		target = rel
	}

	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(disk, []byte("rootfs!"), 0o644))
	require.NoError(t, writeMetadataFile(metadata, &imageMetadata{
		Name: repository + ":" + tag, Digest: "sha256:" + digestHex,
		Status: StatusReady, SizeBytes: int64(len("rootfs!")), CreatedAt: time.Now().UTC(),
	}))
	require.NoError(t, os.MkdirAll(filepath.Dir(linkPath), 0o755))
	require.NoError(t, os.Symlink(target, linkPath))
}

func newTagTestCase(t *testing.T) (*paths.Paths, *manager, string) {
	t.Helper()
	p := paths.New(t.TempDir())
	return p, &manager{paths: p, tagGenerations: make(map[string]uint64)}, "docker.io/library/alpine"
}

func requireTagResolvesTo(t *testing.T, p *paths.Paths, repository, tag, digest string) {
	t.Helper()
	resolved, err := resolveTag(p, repository, tag)
	require.NoError(t, err)
	require.Equal(t, digest, resolved)
}

func tagImage(t *testing.T, m *manager, source, target, digest string) {
	t.Helper()
	img, err := m.TagImage(context.Background(), source, target)
	require.NoError(t, err)
	require.Equal(t, target, img.Name)
	require.Equal(t, "sha256:"+digest, img.Digest)
}

func TestTagImageAliasesReadyImage(t *testing.T) {
	cases := []struct {
		name, source, target, digest string
	}{
		{"same repository", "docker.io/library/alpine:latest", "docker.io/library/alpine:stable", "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"},
		{"digest source", "docker.io/library/alpine@sha256:b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2", "docker.io/library/alpine:pinned", "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, m, repository := newTagTestCase(t)
			seedContent(t, p, repository, "latest", tc.digest)
			tagImage(t, m, tc.source, tc.target, tc.digest)
			targetRef, err := ParseNormalizedRef(tc.target)
			require.NoError(t, err)
			requireTagResolvesTo(t, p, targetRef.Repository(), targetRef.Tag(), tc.digest)
		})
	}
}

func TestTagImagePersistsTargetReference(t *testing.T) {
	p, m, repository := newTagTestCase(t)
	digest := "f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1"
	seedContent(t, p, repository, "latest", digest)

	meta, err := readContentMetadata(p, digest)
	require.NoError(t, err)
	meta.Tags = tags.Tags{"source": "tag"}
	meta.References = map[string]tags.Tags{
		repository + ":latest": {"source": "tag"},
	}
	require.NoError(t, writeMetadataFile(p.ImageContentMetadata(digest), meta))

	target := repository + ":stable"
	tagImage(t, m, repository+":latest", target, digest)

	image, err := m.GetImage(context.Background(), target)
	require.NoError(t, err)
	require.Empty(t, image.Tags)

	meta, err = readContentMetadata(p, digest)
	require.NoError(t, err)
	_, ok := meta.References[target]
	require.True(t, ok, "tagging must persist an explicit target reference")
}

func TestTagImageSameRepositoryDeletesContent(t *testing.T) {
	p, m, repository := newTagTestCase(t)
	digest := "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	seedContent(t, p, repository, "latest", digest)

	tagImage(t, m, repository+":latest", repository+":stable", digest)
	requireTagResolvesTo(t, p, repository, "stable", digest)

	require.NoError(t, m.DeleteImage(context.Background(), repository+":latest"))
	_, err := os.Stat(p.ImageContentDir(digest))
	require.NoError(t, err)
	require.NoError(t, m.DeleteImage(context.Background(), repository+":stable"))
	_, err = os.Stat(p.ImageContentDir(digest))
	require.True(t, os.IsNotExist(err), "content must be removed once unreferenced")
}

func TestTagImageLegacyLayoutSource(t *testing.T) {
	p, m, repository := newTagTestCase(t)
	digest := "d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8"
	seedLegacy(t, p, repository, "latest", digest)

	tagImage(t, m, repository+":latest", repository+":v2", digest)
	requireTagResolvesTo(t, p, repository, "v2", digest)
}

func TestTagImageCrossRepository(t *testing.T) {
	p, m, sourceRepo := newTagTestCase(t)
	targetRepo := "registry.example/apps/alpine"
	digest := "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	seedLegacy(t, p, sourceRepo, "latest", digest)

	tagImage(t, m, sourceRepo+":latest", targetRepo+":v1", digest)
	data, err := os.ReadFile(p.ImageContentPath(digest))
	require.NoError(t, err)
	require.Equal(t, "rootfs!", string(data))
	require.DirExists(t, p.ImageDigestDir(sourceRepo, digest))

	for repo, tag := range map[string]string{sourceRepo: "latest", targetRepo: "v1"} {
		requireTagResolvesTo(t, p, repo, tag, digest)
	}
	require.NoError(t, m.DeleteImage(context.Background(), sourceRepo+":latest"))
	_, err = os.Stat(p.ImageContentDir(digest))
	require.NoError(t, err)
	require.NoError(t, m.DeleteImage(context.Background(), targetRepo+":v1"))
	_, err = os.Stat(p.ImageContentDir(digest))
	require.True(t, os.IsNotExist(err), "content must be removed once unreferenced")
}

func TestStaleTagClaimCollectsContent(t *testing.T) {
	p, m, repository := newTagTestCase(t)
	digest := "f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2"
	seedContent(t, p, repository, "latest", digest)
	linkPath := p.ImageRepositoryTagSymlink(repository, "latest")
	require.NoError(t, os.Remove(linkPath))
	require.NoError(t, os.Symlink("replaced", linkPath))

	normalized, err := ParseNormalizedRef(repository + ":latest")
	require.NoError(t, err)
	m.tagGenerations[repository+":latest"] = 2
	ref := NewResolvedRef(normalized, "sha256:"+digest)
	meta, err := readContentMetadata(p, digest)
	require.NoError(t, err)
	meta.RequestedTag = "latest"
	meta.TagGeneration = 1

	m.claimImageTags(ref, meta)

	_, err = os.Stat(p.ImageContentDir(digest))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestTagImageRejectsNotReady(t *testing.T) {
	p, m, repository := newTagTestCase(t)
	digest := "d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4"
	require.NoError(t, os.MkdirAll(p.ImageContentDir(digest), 0o755))
	require.NoError(t, writeMetadataFile(p.ImageContentMetadata(digest), &imageMetadata{
		Name: repository + ":latest", Digest: "sha256:" + digest,
		Status: StatusConverting, CreatedAt: time.Now().UTC(),
	}))
	require.NoError(t, createTagSymlink(p, repository, "latest", digest))

	targetRepository := "registry.example/apps/alpine"
	_, err := m.TagImage(context.Background(), repository+":latest", targetRepository+":stable")
	require.ErrorIs(t, err, ErrImageNotReady)
	_, err = resolveTag(p, targetRepository, "stable")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestTagImageSourceNotFound(t *testing.T) {
	_, m, repository := newTagTestCase(t)
	_, err := m.TagImage(context.Background(), repository+":missing", repository+":stable")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestTagImageFailureLeavesNoSideEffects(t *testing.T) {
	p, m, repository := newTagTestCase(t)
	digest := "d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5"
	seedContent(t, p, repository, "latest", digest)

	// Failed calls must not create the target tag or bump its generation,
	// which would invalidate a pending pull's claim on the target tag.
	_, err := m.TagImage(context.Background(), repository+":missing", repository+":stable")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = resolveTag(p, repository, "stable")
	require.ErrorIs(t, err, ErrNotFound)
	require.Equal(t, uint64(0), m.tagGenerations[repository+":stable"])

	tagImage(t, m, repository+":latest", repository+":stable", digest)
	requireTagResolvesTo(t, p, repository, "stable", digest)
}

func TestTagImageRejectsDigestTarget(t *testing.T) {
	p, m, repository := newTagTestCase(t)
	digest := "e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5"
	seedContent(t, p, repository, "latest", digest)
	_, err := m.TagImage(context.Background(), repository+":latest", repository+"@sha256:"+digest)
	require.ErrorIs(t, err, ErrInvalidName)
}

func TestTagImageReplacesExistingTagAndCollectsOldContent(t *testing.T) {
	p, m, repository := newTagTestCase(t)
	first := "f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6"
	second := "a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7"
	seedContent(t, p, repository, "latest", first)
	seedContent(t, p, repository, "stable", second)

	readyBytes, err := m.TotalImageBytes(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(14), readyBytes)

	tagImage(t, m, repository+":latest", repository+":stable", first)
	requireTagResolvesTo(t, p, repository, "stable", first)
	_, err = os.Stat(p.ImageContentDir(second))
	require.ErrorIs(t, err, os.ErrNotExist)

	readyBytes, err = m.TotalImageBytes(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(7), readyBytes)
}

func TestTagImageKeepsLegacySiblingTags(t *testing.T) {
	p, m, repository := newTagTestCase(t)
	first := "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	second := "d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4"
	seedLegacy(t, p, repository, "latest", first)
	seedLegacy(t, p, repository, "stable", first)
	seedLegacy(t, p, repository, "v2", second)

	tagImage(t, m, repository+":v2", repository+":stable", second)
	requireTagResolvesTo(t, p, repository, "stable", second)
	requireTagResolvesTo(t, p, repository, "latest", first)
	_, err := os.Stat(p.ImageDigestDir(repository, first))
	require.NoError(t, err, "legacy content for a sibling tag must not be collected")
}

func TestTagImageSelfHealsDanglingTargetSymlink(t *testing.T) {
	p, m, repository := newTagTestCase(t)
	digest := "e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6"
	seedContent(t, p, repository, "latest", digest)

	linkPath := p.ImageRepositoryTagSymlink(repository, "stable")
	require.NoError(t, os.MkdirAll(filepath.Dir(linkPath), 0o755))
	require.NoError(t, os.Symlink("already-collected", linkPath))

	tagImage(t, m, repository+":latest", repository+":stable", digest)
	requireTagResolvesTo(t, p, repository, "stable", digest)
}
