package images

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestDeleteTagRemovesBothLayoutReferences(t *testing.T) {
	p := paths.New(t.TempDir())
	repository := "docker.io/library/alpine"
	tag := "latest"
	digest := "abababababababababababababababababababababababababababababababab"
	legacyPath := p.ImageTagSymlink(repository, tag)
	contentPath := p.ImageRepositoryTagSymlink(repository, tag)
	require.NoError(t, os.MkdirAll(filepath.Dir(legacyPath), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(contentPath), 0o755))
	require.NoError(t, os.Symlink(digest, legacyPath))
	target, err := filepath.Rel(filepath.Dir(contentPath), p.ImageContentDir(digest))
	require.NoError(t, err)
	require.NoError(t, os.Symlink(target, contentPath))

	require.NoError(t, deleteTag(p, repository, tag))
	_, err = os.Lstat(legacyPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Lstat(contentPath)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestLegacyImageIsNotShadowedByContentMetadata(t *testing.T) {
	p := paths.New(t.TempDir())
	repository := "docker.io/library/alpine"
	digest := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	legacyMeta := &imageMetadata{
		Name:   repository + ":latest",
		Digest: "sha256:" + digest,
		Status: StatusReady,
	}
	legacyDir := p.ImageDigestDir(repository, digest)
	require.NoError(t, os.MkdirAll(legacyDir, 0o755))
	require.NoError(t, writeMetadataFile(p.ImageMetadata(repository, digest), legacyMeta))
	require.NoError(t, os.WriteFile(p.ImageDigestPath(repository, digest), []byte("legacy rootfs"), 0o644))
	require.NoError(t, writeMetadataFile(p.ImageContentMetadata(digest), &imageMetadata{
		Name:   "docker.io/library/busybox:latest",
		Digest: "sha256:" + digest,
		Status: StatusFailed,
	}))

	meta, err := readMetadata(p, repository, digest)
	require.NoError(t, err)
	require.Equal(t, StatusReady, meta.Status)
	require.Equal(t, p.ImageDigestPath(repository, digest), digestPath(p, repository, digest))
}

func TestListAllMetadataDeduplicatesDualLayouts(t *testing.T) {
	p := paths.New(t.TempDir())
	repository := "docker.io/library/alpine"
	digest := "9999999999999999999999999999999999999999999999999999999999999999"
	meta := &imageMetadata{
		Name:   repository + "@sha256:" + digest,
		Digest: "sha256:" + digest,
		Status: StatusReady,
	}

	require.NoError(t, os.MkdirAll(p.ImageDigestDir(repository, digest), 0o755))
	require.NoError(t, writeMetadataFile(p.ImageMetadata(repository, digest), meta))
	require.NoError(t, os.WriteFile(p.ImageDigestPath(repository, digest), []byte("legacy rootfs"), 0o644))
	require.NoError(t, writeMetadataFile(p.ImageContentMetadata(digest), meta))
	require.NoError(t, os.WriteFile(p.ImageContentPath(digest), []byte("content rootfs"), 0o644))

	metas, err := listAllMetadata(p)
	require.NoError(t, err)
	require.Len(t, metas, 1)
	require.Equal(t, meta.Digest, metas[0].Digest)
}

func TestFailedLegacyImageUsesReadyContent(t *testing.T) {
	p := paths.New(t.TempDir())
	repository := "docker.io/library/alpine"
	digest := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	legacyDir := p.ImageDigestDir(repository, digest)
	require.NoError(t, os.MkdirAll(legacyDir, 0o755))
	require.NoError(t, writeMetadataFile(p.ImageMetadata(repository, digest), &imageMetadata{
		Name:   repository + ":latest",
		Digest: "sha256:" + digest,
		Status: StatusFailed,
	}))
	require.NoError(t, os.WriteFile(p.ImageDigestPath(repository, digest), []byte("legacy rootfs"), 0o644))
	require.NoError(t, writeMetadataFile(p.ImageContentMetadata(digest), &imageMetadata{
		Name:      "registry.example.com/app:latest",
		Digest:    "sha256:" + digest,
		Status:    StatusReady,
		SizeBytes: 13,
	}))
	require.NoError(t, os.WriteFile(p.ImageContentPath(digest), []byte("content rootfs"), 0o644))

	meta, err := readMetadata(p, repository, digest)
	require.NoError(t, err)
	require.Equal(t, StatusReady, meta.Status)
	require.Equal(t, p.ImageContentDir(digest), digestDir(p, repository, digest))
	require.Equal(t, p.ImageContentPath(digest), digestPath(p, repository, digest))
}

func TestReadyContentDoesNotFallBackToLegacyDisk(t *testing.T) {
	p := paths.New(t.TempDir())
	repository := "docker.io/library/alpine"
	digest := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	legacyDir := p.ImageDigestDir(repository, digest)
	require.NoError(t, os.MkdirAll(legacyDir, 0o755))
	require.NoError(t, writeMetadataFile(p.ImageMetadata(repository, digest), &imageMetadata{
		Name:   repository + ":latest",
		Digest: "sha256:" + digest,
		Status: StatusReady,
	}))
	require.NoError(t, os.WriteFile(p.ImageDigestPath(repository, digest), []byte("legacy rootfs"), 0o644))
	require.NoError(t, writeMetadataFile(p.ImageContentMetadata(digest), &imageMetadata{
		Name:   repository + ":latest",
		Digest: "sha256:" + digest,
		Status: StatusReady,
	}))

	require.Equal(t, p.ImageContentMetadata(digest), metadataPath(p, repository, digest))
	require.Equal(t, p.ImageContentPath(digest), digestPath(p, repository, digest))
	_, err := readMetadata(p, repository, digest)
	require.ErrorContains(t, err, "disk image missing")
}

func TestWriteMetadataUsesContentWhenLegacyDirectoryIsEmpty(t *testing.T) {
	p := paths.New(t.TempDir())
	repository := "docker.io/library/alpine"
	digest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	require.NoError(t, os.MkdirAll(p.ImageDigestDir(repository, digest), 0o755))
	require.NoError(t, writeMetadata(p, repository, digest, &imageMetadata{
		Name:   repository + ":latest",
		Digest: "sha256:" + digest,
		Status: StatusPending,
	}))
	require.FileExists(t, p.ImageContentMetadata(digest))
	_, err := os.Stat(p.ImageMetadata(repository, digest))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestContentLayoutResolvesDiskByDigest(t *testing.T) {
	p := paths.New(t.TempDir())
	repository := "docker.io/library/alpine"
	digest := "1212121212121212121212121212121212121212121212121212121212121212"
	meta := &imageMetadata{
		Name:   repository + ":latest",
		Digest: "sha256:" + digest,
		Status: StatusReady,
	}
	require.NoError(t, writeMetadataFile(p.ImageContentMetadata(digest), meta))
	require.NoError(t, os.WriteFile(p.ImageContentPath(digest), []byte("rootfs"), 0o644))

	got, err := GetDiskPath(p, repository+":latest", "sha256:"+digest)
	require.NoError(t, err)
	require.Equal(t, p.ImageContentPath(digest), got)
}

func TestListAllMetadataContentLayout(t *testing.T) {
	p := paths.New(t.TempDir())
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	meta := &imageMetadata{
		Name:      "docker.io/library/alpine:latest",
		Digest:    "sha256:" + digest,
		Status:    StatusReady,
		SizeBytes: 5,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, writeMetadataFile(p.ImageContentMetadata(digest), meta))
	require.NoError(t, os.WriteFile(p.ImageContentPath(digest), []byte("rootfs"), 0o644))
	linkPath := p.ImageRepositoryTagSymlink("docker.io/library/alpine", "latest")
	require.NoError(t, os.MkdirAll(filepath.Dir(linkPath), 0o755))
	target, err := filepath.Rel(filepath.Dir(linkPath), p.ImageContentDir(digest))
	require.NoError(t, err)
	require.NoError(t, os.Symlink(target, linkPath))
	require.NoError(t, createTagSymlink(p, "docker.io/library/alpine", "stable", digest))
	_, err = os.Lstat(p.ImageRepositoryTagSymlink("docker.io/library/alpine", "stable"))
	require.NoError(t, err)

	metas, err := listAllMetadata(p)
	require.NoError(t, err)
	require.Len(t, metas, 2)
	names := []string{metas[0].Name, metas[1].Name}
	require.ElementsMatch(t, []string{
		"docker.io/library/alpine:latest",
		"docker.io/library/alpine:stable",
	}, names)
}

func TestImageMetadataToImage_ClonesMetadata(t *testing.T) {
	createdAt := time.Now().UTC().Truncate(time.Second)
	source := &imageMetadata{
		Name:      "docker.io/library/alpine:latest",
		Digest:    "sha256:abc",
		Status:    StatusReady,
		Tags:      map[string]string{"team": "backend", "env": "staging"},
		SizeBytes: 123,
		CreatedAt: createdAt,
	}

	img := source.toImageFor(source.Name)
	require.Equal(t, source.Name, img.Name)
	require.Equal(t, source.Digest, img.Digest)
	require.Equal(t, map[string]string{"team": "backend", "env": "staging"}, img.Tags)
	require.NotNil(t, img.SizeBytes)
	require.Equal(t, int64(123), *img.SizeBytes)

	source.Tags["team"] = "mutated"
	require.Equal(t, "backend", img.Tags["team"])
}

func TestImageMetadataToImage_EmptyMetadataOmitted(t *testing.T) {
	img := (&imageMetadata{
		Name:      "docker.io/library/alpine:latest",
		Digest:    "sha256:abc",
		Status:    StatusPending,
		CreatedAt: time.Now().UTC(),
	}).toImageFor("docker.io/library/alpine:latest")

	require.Nil(t, img.Tags)
}

func TestNewManagerLeavesLegacyImagesInPlace(t *testing.T) {
	p := paths.New(t.TempDir())
	repository := "docker.io/library/alpine"
	tag := "latest"
	digest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	legacyDir := p.ImageDigestDir(repository, digest)
	require.NoError(t, os.MkdirAll(legacyDir, 0o755))
	require.NoError(t, writeMetadataFile(p.ImageMetadata(repository, digest), &imageMetadata{
		Name: repository + ":" + tag, Digest: "sha256:" + digest,
		Status: StatusReady, SizeBytes: 7, CreatedAt: time.Now().UTC(),
	}))
	require.NoError(t, os.WriteFile(p.ImageDigestPath(repository, digest), []byte("rootfs!"), 0o644))
	tagPath := p.ImageTagSymlink(repository, tag)
	require.NoError(t, os.MkdirAll(filepath.Dir(tagPath), 0o755))
	require.NoError(t, os.Symlink(digest, tagPath))

	_, err := NewManager(p, 1, nil)
	require.NoError(t, err)
	require.DirExists(t, legacyDir)
	require.FileExists(t, p.ImageMetadata(repository, digest))
	require.FileExists(t, p.ImageDigestPath(repository, digest))
	_, err = os.Stat(p.ImageContentDir(digest))
	require.ErrorIs(t, err, os.ErrNotExist)
	requireTagResolvesTo(t, p, repository, tag, digest)
}

func TestPromoteLegacyImagesMovesContentAndTags(t *testing.T) {
	p := paths.New(t.TempDir())
	repository := "docker.io/library/alpine"
	tag := "latest"
	digest := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	// Ready legacy image with a legacy tag symlink.
	legacyDir := p.ImageDigestDir(repository, digest)
	require.NoError(t, os.MkdirAll(legacyDir, 0o755))
	meta := &imageMetadata{
		Name:      repository + ":" + tag,
		Digest:    "sha256:" + digest,
		Status:    StatusReady,
		SizeBytes: 7,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, writeMetadataFile(p.ImageMetadata(repository, digest), meta))
	require.NoError(t, os.WriteFile(p.ImageDigestPath(repository, digest), []byte("rootfs!"), 0o644))
	tagPath := p.ImageTagSymlink(repository, tag)
	require.NoError(t, os.MkdirAll(filepath.Dir(tagPath), 0o755))
	require.NoError(t, os.Symlink(digest, tagPath))

	refs, err := collectLegacyImages(p)
	require.NoError(t, err)
	promoteLegacyImages(p, refs)

	// Content exists with the same bytes and is ready.
	contentMeta, err := readContentMetadata(p, digest)
	require.NoError(t, err)
	require.Equal(t, StatusReady, contentMeta.Status)
	data, err := os.ReadFile(p.ImageContentPath(digest))
	require.NoError(t, err)
	require.Equal(t, "rootfs!", string(data))

	require.FileExists(t, p.ImageDigestPath(repository, digest))

	// The tag now resolves through the shared content layout.
	resolved, err := resolveTag(p, repository, tag)
	require.NoError(t, err)
	require.Equal(t, digest, resolved)
}

func TestPromoteLegacyImagesSkipsNonReady(t *testing.T) {
	p := paths.New(t.TempDir())
	repository := "docker.io/library/alpine"
	digest := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	legacyDir := p.ImageDigestDir(repository, digest)
	require.NoError(t, os.MkdirAll(legacyDir, 0o755))
	require.NoError(t, writeMetadataFile(p.ImageMetadata(repository, digest), &imageMetadata{
		Name:      repository + ":latest",
		Digest:    "sha256:" + digest,
		Status:    StatusConverting,
		CreatedAt: time.Now().UTC(),
	}))

	refs, err := collectLegacyImages(p)
	require.NoError(t, err)
	promoteLegacyImages(p, refs)

	_, err = os.Stat(p.ImageContentMetadata(digest))
	require.True(t, os.IsNotExist(err), "non-ready legacy image must not be promoted")
	_, err = os.Stat(legacyDir)
	require.NoError(t, err, "non-ready legacy tree must be preserved")
}
