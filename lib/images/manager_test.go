package images

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConversionFailedErr(t *testing.T) {
	t.Run("without detail", func(t *testing.T) {
		assert.EqualError(t, conversionFailedErr(nil, nil), "image conversion failed")
	})

	t.Run("with stored message", func(t *testing.T) {
		message := "converter exited"
		assert.EqualError(t, conversionFailedErr(&message, nil), "image conversion failed: converter exited")
	})

	t.Run("with cause", func(t *testing.T) {
		cause := errors.New("converter exited")
		err := conversionFailedErr(nil, cause)
		assert.EqualError(t, err, "image conversion failed: converter exited")
		assert.ErrorIs(t, err, cause)
	})
}

func TestCreateImage(t *testing.T) {
	dataDir := t.TempDir()
	mgr, err := NewManager(paths.New(dataDir), 1, nil)
	require.NoError(t, err)

	ctx := context.Background()
	req := CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	img, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, img)
	require.Equal(t, "docker.io/library/alpine:latest", img.Name)

	waitForReady(t, mgr, ctx, img.Name)

	img, err = mgr.GetImage(ctx, img.Name)
	require.NoError(t, err)
	require.Equal(t, StatusReady, img.Status)
	require.NotNil(t, img.SizeBytes)
	require.Greater(t, *img.SizeBytes, int64(0))
	require.NotEmpty(t, img.Digest)
	require.Contains(t, img.Digest, "sha256:")

	// Check that digest directory exists
	ref, err := ParseNormalizedRef(img.Name)
	require.NoError(t, err)
	digestHex := strings.SplitN(img.Digest, ":", 2)[1]

	// Check rootfs disk file (erofs on Linux, ext4 on Darwin)
	diskPath := digestPath(paths.New(dataDir), ref.Repository(), digestHex)
	diskStat, err := os.Stat(diskPath)
	require.NoError(t, err)
	require.False(t, diskStat.IsDir(), "disk path should be a file")
	require.Greater(t, diskStat.Size(), int64(1000000), "rootfs disk should be at least 1MB")
	require.Equal(t, diskStat.Size(), *img.SizeBytes, "disk size should match metadata")
	t.Logf("Rootfs disk (%s): path=%s, size=%d bytes", DefaultImageFormat, diskPath, diskStat.Size())

	// Check metadata file
	metadataPath := metadataPath(paths.New(dataDir), ref.Repository(), digestHex)
	metaStat, err := os.Stat(metadataPath)
	require.NoError(t, err)
	require.False(t, metaStat.IsDir(), "metadata should be a file")

	// Read and verify metadata content
	meta, err := readMetadata(paths.New(dataDir), ref.Repository(), digestHex)
	require.NoError(t, err)
	require.Equal(t, img.Name, meta.Name)
	require.Equal(t, img.Digest, meta.Digest)
	require.Equal(t, StatusReady, meta.Status)
	require.Nil(t, meta.Error)
	require.Equal(t, diskStat.Size(), meta.SizeBytes)
	require.NotEmpty(t, meta.Env, "should have environment variables")
	t.Logf("Metadata: name=%s, digest=%s, status=%s, env_vars=%d",
		meta.Name, meta.Digest, meta.Status, len(meta.Env))

	// Check that tag symlink exists and points to correct digest
	linkPath := tagSymlinkPath(paths.New(dataDir), ref.Repository(), ref.Tag())
	linkStat, err := os.Lstat(linkPath)
	require.NoError(t, err)
	require.NotEqual(t, 0, linkStat.Mode()&os.ModeSymlink, "should be a symlink")

	// Verify symlink points to digest directory
	linkTarget, err := os.Readlink(linkPath)
	require.NoError(t, err)
	require.Equal(t, digestHex, linkTarget, "symlink should point to digest")
	t.Logf("Tag symlink: %s -> %s", linkPath, linkTarget)
}

func TestCreateImageDifferentTag(t *testing.T) {
	dataDir := t.TempDir()
	mgr, err := NewManager(paths.New(dataDir), 1, nil)
	require.NoError(t, err)

	ctx := context.Background()
	req := CreateImageRequest{
		Name: "docker.io/library/alpine:3.18",
	}

	img, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, img)
	require.Equal(t, "docker.io/library/alpine:3.18", img.Name)

	waitForReady(t, mgr, ctx, img.Name)

	img, err = mgr.GetImage(ctx, img.Name)
	require.NoError(t, err)
	require.NotEmpty(t, img.Digest)
}

func TestCreateImageDuplicate(t *testing.T) {
	dataDir := t.TempDir()
	mgr, err := NewManager(paths.New(dataDir), 1, nil)
	require.NoError(t, err)

	ctx := context.Background()
	req := CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	img1, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)

	waitForReady(t, mgr, ctx, img1.Name)

	// Re-fetch img1 to get the complete metadata including digest
	img1, err = mgr.GetImage(ctx, img1.Name)
	require.NoError(t, err)
	require.NotEmpty(t, img1.Digest)

	// Second create should be idempotent and return existing image
	img2, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, img2)
	require.Equal(t, img1.Name, img2.Name)
	require.Equal(t, StatusReady, img2.Status)
	require.Equal(t, img1.Digest, img2.Digest) // Same digest
}

func TestTagFollowsLastPull(t *testing.T) {
	// Docker last-pull-wins: a tag is always repointed to the most recently
	// pulled digest regardless of platform. This is the HIGH-2 guarantee -- an
	// emulated (amd64-on-arm64) pull must be able to move the tag back after a
	// host-native pull moved it. Drive createTagSymlink directly with two
	// distinct digests in both orders and assert resolveTag follows the latest.
	const (
		repo   = "docker.io/library/alpine"
		tag    = "3.19"
		native = "1111111111111111111111111111111111111111111111111111111111111111"
		emul   = "2222222222222222222222222222222222222222222222222222222222222222"
	)

	check := func(t *testing.T, first, second string) {
		p := paths.New(t.TempDir())

		require.NoError(t, createTagSymlink(p, repo, tag, first))
		got, err := resolveTag(p, repo, tag)
		require.NoError(t, err)
		require.Equal(t, first, got)

		// A later pull of the same tag (e.g. the emulated variant) must repoint.
		require.NoError(t, createTagSymlink(p, repo, tag, second))
		got, err = resolveTag(p, repo, tag)
		require.NoError(t, err)
		require.Equal(t, second, got, "tag must follow the most recent pull")
	}

	t.Run("native then emulated", func(t *testing.T) { check(t, native, emul) })
	t.Run("emulated then native", func(t *testing.T) { check(t, emul, native) })
}

func TestTagImage(t *testing.T) {
	p := paths.New(t.TempDir())
	mgr, err := NewManager(p, 1, nil)
	require.NoError(t, err)

	const (
		repo      = "docker.io/library/alpine"
		digestRef = repo + "@sha256:029a752048e32e843bd6defe3841186fb8d19a28dae8ec287f433bb9d6d1ad85"
	)
	seedReadyDigestOnlyImageMetadata(t, p, digestRef, nil)

	ctx := context.Background()
	got, err := mgr.TagImage(ctx, digestRef, repo+":stable")
	require.NoError(t, err)
	require.Equal(t, repo+":stable", got.Name)
	require.Equal(t, "sha256:029a752048e32e843bd6defe3841186fb8d19a28dae8ec287f433bb9d6d1ad85", got.Digest)
	resolved, err := mgr.GetImage(ctx, repo+":stable")
	require.NoError(t, err)
	require.Equal(t, repo+":stable", resolved.Name)
	require.Equal(t, StatusReady, resolved.Status)
	require.Equal(t, got.Digest, resolved.Digest)

	aliased, err := mgr.TagImage(ctx, digestRef, "docker.io/library/busybox:stable")
	require.NoError(t, err)
	require.Equal(t, "docker.io/library/busybox:stable", aliased.Name)
	resolvedAlias, err := mgr.GetImage(ctx, "docker.io/library/busybox:stable")
	require.NoError(t, err)
	require.Equal(t, "docker.io/library/busybox:stable", resolvedAlias.Name)
	require.Equal(t, got.Digest, resolvedAlias.Digest)

	require.NoError(t, mgr.DeleteImage(ctx, digestRef))
	resolvedAlias, err = mgr.GetImage(ctx, "docker.io/library/busybox:stable")
	require.NoError(t, err)
	require.Equal(t, "docker.io/library/busybox:stable", resolvedAlias.Name)
	require.Equal(t, got.Digest, resolvedAlias.Digest)

	_, err = mgr.TagImage(ctx, digestRef, repo+"@sha256:029a752048e32e843bd6defe3841186fb8d19a28dae8ec287f433bb9d6d1ad85")
	require.ErrorIs(t, err, ErrInvalidName)
}

func TestDigestOnlyContentSurvivesAliasDeletion(t *testing.T) {
	p := paths.New(t.TempDir())
	mgr, err := NewManager(p, 1, nil)
	require.NoError(t, err)

	repository := "docker.io/library/alpine"
	digest := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	require.NoError(t, writeMetadataFile(p.ImageContentMetadata(digest), &imageMetadata{
		Name:      repository + "@sha256:" + digest,
		Digest:    "sha256:" + digest,
		Status:    StatusReady,
		SizeBytes: 5,
		CreatedAt: time.Now().UTC(),
	}))
	require.NoError(t, os.WriteFile(p.ImageContentPath(digest), []byte("rootfs"), 0o644))
	linkPath := p.ImageRepositoryTagSymlink("registry.example.com/myapp", "v1")
	require.NoError(t, os.MkdirAll(filepath.Dir(linkPath), 0o755))
	target, err := filepath.Rel(filepath.Dir(linkPath), p.ImageContentDir(digest))
	require.NoError(t, err)
	require.NoError(t, os.Symlink(target, linkPath))

	require.NoError(t, mgr.DeleteImage(context.Background(), "registry.example.com/myapp:v1"))
	_, err = mgr.GetImage(context.Background(), repository+"@sha256:"+digest)
	require.NoError(t, err)

	require.NoError(t, mgr.DeleteImage(context.Background(), repository+"@sha256:"+digest))
	_, err = os.Stat(p.ImageContentDir(digest))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestListImages(t *testing.T) {
	dataDir := t.TempDir()
	mgr, err := NewManager(paths.New(dataDir), 1, nil)
	require.NoError(t, err)

	ctx := context.Background()

	// Initially empty
	images, err := mgr.ListImages(ctx)
	require.NoError(t, err)
	require.Len(t, images, 0)

	req1 := CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}
	img1, err := mgr.CreateImage(ctx, req1)
	require.NoError(t, err)

	waitForReady(t, mgr, ctx, img1.Name)

	// List should return one image
	images, err = mgr.ListImages(ctx)
	require.NoError(t, err)
	require.Len(t, images, 1)
	require.Equal(t, "docker.io/library/alpine:latest", images[0].Name)
	require.Equal(t, StatusReady, images[0].Status)
	require.NotEmpty(t, images[0].Digest)
}

func TestListImages_IncludesDigestOnlyImages(t *testing.T) {
	dataDir := t.TempDir()
	p := paths.New(dataDir)
	mgr, err := NewManager(p, 1, nil)
	require.NoError(t, err)

	const digestRef = "docker.io/library/alpine@sha256:029a752048e32e843bd6defe3841186fb8d19a28dae8ec287f433bb9d6d1ad85"
	seedReadyDigestOnlyImageMetadata(t, p, digestRef, map[string]string{
		"qa":      "pr43-qa-20260323134902",
		"surface": "image",
	})

	images, err := mgr.ListImages(context.Background())
	require.NoError(t, err)
	require.Len(t, images, 1)
	require.Equal(t, digestRef, images[0].Name)
	require.Equal(t, map[string]string{
		"qa":      "pr43-qa-20260323134902",
		"surface": "image",
	}, map[string]string(images[0].Tags))
}

func TestGetImage(t *testing.T) {
	dataDir := t.TempDir()
	mgr, err := NewManager(paths.New(dataDir), 1, nil)
	require.NoError(t, err)

	ctx := context.Background()
	req := CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	created, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)

	waitForReady(t, mgr, ctx, created.Name)

	img, err := mgr.GetImage(ctx, created.Name)
	require.NoError(t, err)
	require.NotNil(t, img)
	require.Equal(t, created.Name, img.Name)
	require.Equal(t, StatusReady, img.Status)
	require.NotNil(t, img.SizeBytes)
	require.NotEmpty(t, img.Digest)
}

func TestGetImageNotFound(t *testing.T) {
	dataDir := t.TempDir()
	mgr, err := NewManager(paths.New(dataDir), 1, nil)
	require.NoError(t, err)

	ctx := context.Background()

	_, err = mgr.GetImage(ctx, "nonexistent:latest")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestDeleteImage(t *testing.T) {
	dataDir := t.TempDir()
	mgr, err := NewManager(paths.New(dataDir), 1, nil)
	require.NoError(t, err)

	ctx := context.Background()
	req := CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	created, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)

	waitForReady(t, mgr, ctx, created.Name)

	// Get the digest before deleting
	img, err := mgr.GetImage(ctx, created.Name)
	require.NoError(t, err)
	ref, err := ParseNormalizedRef(img.Name)
	require.NoError(t, err)
	digestHex := strings.SplitN(img.Digest, ":", 2)[1]

	err = mgr.DeleteImage(ctx, created.Name)
	require.NoError(t, err)

	// Tag should be gone
	_, err = mgr.GetImage(ctx, created.Name)
	require.ErrorIs(t, err, ErrNotFound)

	// Digest directory should also be deleted (no orphaned digests)
	digestDir := digestPath(paths.New(dataDir), ref.Repository(), digestHex)
	_, err = os.Stat(digestDir)
	require.True(t, os.IsNotExist(err), "digest directory should be deleted when orphaned")
}

func TestDeleteImageByDigest(t *testing.T) {
	dataDir := t.TempDir()
	p := paths.New(dataDir)
	mgr, err := NewManager(p, 1, nil)
	require.NoError(t, err)

	ctx := context.Background()
	const digestRef = "docker.io/library/alpine@sha256:029a752048e32e843bd6defe3841186fb8d19a28dae8ec287f433bb9d6d1ad85"
	seedReadyDigestOnlyImageMetadata(t, p, digestRef, map[string]string{
		"qa": "pr43-delete-20260323134902",
	})

	err = mgr.DeleteImage(ctx, digestRef)
	require.NoError(t, err)

	ref, err := ParseNormalizedRef(digestRef)
	require.NoError(t, err)
	_, err = os.Stat(digestDir(p, ref.Repository(), ref.DigestHex()))
	require.True(t, os.IsNotExist(err), "digest directory should be deleted when deleting by digest")
}

func TestDeleteImageNotFound(t *testing.T) {
	dataDir := t.TempDir()
	mgr, err := NewManager(paths.New(dataDir), 1, nil)
	require.NoError(t, err)

	ctx := context.Background()

	err = mgr.DeleteImage(ctx, "nonexistent:latest")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestDeleteImagePreservesSharedDigest(t *testing.T) {
	dataDir := t.TempDir()
	p := paths.New(dataDir)
	mgr, err := NewManager(p, 1, nil)
	require.NoError(t, err)

	ctx := context.Background()
	req := CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	}

	created, err := mgr.CreateImage(ctx, req)
	require.NoError(t, err)

	waitForReady(t, mgr, ctx, created.Name)

	// Get the digest
	img, err := mgr.GetImage(ctx, created.Name)
	require.NoError(t, err)
	ref, err := ParseNormalizedRef(img.Name)
	require.NoError(t, err)
	digestHex := strings.SplitN(img.Digest, ":", 2)[1]

	// Create a second tag pointing to the same digest
	err = createTagSymlink(p, ref.Repository(), "v1.0", digestHex)
	require.NoError(t, err)

	// Delete the first tag
	err = mgr.DeleteImage(ctx, created.Name)
	require.NoError(t, err)

	// First tag should be gone
	_, err = mgr.GetImage(ctx, created.Name)
	require.ErrorIs(t, err, ErrNotFound)

	// Digest directory should still exist (shared by v1.0 tag)
	digestDir := digestPath(p, ref.Repository(), digestHex)
	_, err = os.Stat(digestDir)
	require.NoError(t, err, "digest directory should be preserved when other tags reference it")

	// Second tag should still work
	img2, err := mgr.GetImage(ctx, "docker.io/library/alpine:v1.0")
	require.NoError(t, err)
	assert.Equal(t, img.Digest, img2.Digest)

	// Now delete the second tag
	err = mgr.DeleteImage(ctx, "docker.io/library/alpine:v1.0")
	require.NoError(t, err)

	// Now the digest directory should be deleted
	_, err = os.Stat(digestDir)
	require.True(t, os.IsNotExist(err), "digest directory should be deleted when last tag is removed")
}

func TestNormalizedRefParsing(t *testing.T) {
	tests := []struct {
		input      string
		expectRepo string
		expectTag  string
	}{
		{"alpine", "docker.io/library/alpine", "latest"},
		{"alpine:3.18", "docker.io/library/alpine", "3.18"},
		{"docker.io/library/alpine:latest", "docker.io/library/alpine", "latest"},
		{"ghcr.io/myorg/myapp:v1.0.0", "ghcr.io/myorg/myapp", "v1.0.0"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			ref, err := ParseNormalizedRef(tt.input)
			require.NoError(t, err)

			repo := ref.Repository()
			require.Equal(t, tt.expectRepo, repo)

			tag := ref.Tag()
			require.Equal(t, tt.expectTag, tag)
		})
	}
}

func TestLayerCaching(t *testing.T) {
	dataDir := t.TempDir()
	mgr, err := NewManager(paths.New(dataDir), 1, nil)
	require.NoError(t, err)
	ctx := context.Background()

	// 1. Pull alpine:latest by tag
	t.Log("Pulling alpine:latest by tag...")
	alpine1, err := mgr.CreateImage(ctx, CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	})
	require.NoError(t, err)
	require.NotEmpty(t, alpine1.Digest, "should have digest")

	// Wait for first pull to complete (poll by digest)
	alpine1Ref := "docker.io/library/alpine@" + alpine1.Digest
	waitForReady(t, mgr, ctx, alpine1Ref)

	// Count blobs after first pull
	blobsDir := filepath.Join(dataDir, "system", "oci-cache", "blobs", "sha256")
	blobsAfterFirst, err := countFiles(blobsDir)
	require.NoError(t, err)
	t.Logf("Blobs after first pull: %d", blobsAfterFirst)
	require.Greater(t, blobsAfterFirst, 0, "should have downloaded blobs")

	// 2. Pull the SAME digest but reference it by digest
	// This guarantees 100% layer overlap - tests cross-reference caching
	t.Logf("Pulling same image by digest reference: %s", alpine1.Digest)
	alpine2, err := mgr.CreateImage(ctx, CreateImageRequest{
		Name: alpine1Ref, // Pull by digest instead of tag
	})
	require.NoError(t, err)
	require.Equal(t, alpine1.Digest, alpine2.Digest, "should have same digest")

	// This should be instant - already cached
	waitForReady(t, mgr, ctx, alpine1Ref)

	// Count blobs after second pull
	blobsAfterSecond, err := countFiles(blobsDir)
	require.NoError(t, err)
	t.Logf("Blobs after second pull: %d", blobsAfterSecond)

	// 3. Verify layer caching worked - should add ZERO new blobs
	blobsAdded := blobsAfterSecond - blobsAfterFirst
	require.Equal(t, 0, blobsAdded,
		"Pulling same digest with different reference should not download any new blobs (everything cached)")

	// 4. Verify both references work and point to functional images
	alpine1Parsed, err := ParseNormalizedRef(alpine1.Name)
	require.NoError(t, err)
	alpine2Parsed, err := ParseNormalizedRef(alpine2.Name)
	require.NoError(t, err)

	// Both should point to the same digest directory
	digestHex := strings.TrimPrefix(alpine1.Digest, "sha256:")
	disk1 := digestPath(paths.New(dataDir), alpine1Parsed.Repository(), digestHex)
	disk2 := digestPath(paths.New(dataDir), alpine2Parsed.Repository(), digestHex)

	require.Equal(t, disk1, disk2, "both references should point to same disk")

	stat, err := os.Stat(disk1)
	require.NoError(t, err)
	require.Greater(t, stat.Size(), int64(0))

	t.Logf("Layer caching verified: second pull reused all %d cached blobs", blobsAfterFirst)
}

// countFiles counts the number of files in a directory
func countFiles(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}

func seedReadyDigestOnlyImageMetadata(t *testing.T, p *paths.Paths, imageRef string, imageTags map[string]string) {
	t.Helper()

	ref, err := ParseNormalizedRef(imageRef)
	require.NoError(t, err)
	require.True(t, ref.IsDigest(), "test helper expects a digest reference")

	digestDirPath := p.ImageDigestDir(ref.Repository(), ref.DigestHex())
	require.NoError(t, os.MkdirAll(digestDirPath, 0o755))
	require.NoError(t, os.WriteFile(p.ImageDigestPath(ref.Repository(), ref.DigestHex()), []byte("rootfs"), 0o644))

	meta := struct {
		Name      string            `json:"name"`
		Digest    string            `json:"digest"`
		Status    string            `json:"status"`
		SizeBytes int64             `json:"size_bytes"`
		Tags      map[string]string `json:"tags,omitempty"`
		CreatedAt time.Time         `json:"created_at"`
	}{
		Name:      imageRef,
		Digest:    ref.Digest(),
		Status:    StatusReady,
		SizeBytes: int64(len("rootfs")),
		Tags:      imageTags,
		CreatedAt: time.Now().UTC(),
	}

	data, err := json.Marshal(meta)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p.ImageMetadata(ref.Repository(), ref.DigestHex()), data, 0o644))
}

// TestImportLocalImageFromOCICache is an integration test that simulates the full
// builder image import flow used by buildBuilderFromDockerfile:
//
// 1. Create a synthetic Docker image (simulates docker build output)
// 2. Write it to the OCI layout cache with digest annotation (simulates buildBuilderFromDockerfile)
// 3. Call ImportLocalImage (what buildBuilderFromDockerfile calls after writing to cache)
// 4. Wait for the image to become ready (async build pipeline)
// 5. Verify GetImage returns correct metadata (entrypoint, workdir, env)
// 6. Verify GetDiskPath returns path to a valid ext4 disk file
//
// This proves the end-to-end flow: OCI cache write → ImportLocalImage → buildImage
// → pullAndExport (cache hit) → ExportRootfs → ready.
func TestImportLocalImageFromOCICache(t *testing.T) {
	dataDir := t.TempDir()
	p := paths.New(dataDir)
	mgr, err := NewManager(p, 1, nil)
	require.NoError(t, err)

	ctx := context.Background()

	// Step 1: Create synthetic Docker image
	img := createTestDockerImage(t)

	imgDigest, err := img.Digest()
	require.NoError(t, err)
	digestStr := imgDigest.String() // "sha256:abc123..."
	layoutTag := digestToLayoutTag(digestStr)

	// Step 2: Write to OCI layout cache (same path the image manager uses)
	cacheDir := p.SystemOCICache()
	require.NoError(t, os.MkdirAll(cacheDir, 0755))

	path, err := layout.Write(cacheDir, empty.Index)
	require.NoError(t, err)

	err = path.AppendImage(img, layout.WithAnnotations(map[string]string{
		"org.opencontainers.image.ref.name": layoutTag,
	}))
	require.NoError(t, err)
	t.Logf("Wrote image to OCI cache: digest=%s, layoutTag=%s", digestStr, layoutTag)

	// Step 3: Call ImportLocalImage (what buildBuilderFromDockerfile does)
	imported, err := mgr.ImportLocalImage(ctx, "localhost:8080/internal/builder", "latest", digestStr)
	require.NoError(t, err)
	require.NotNil(t, imported)
	require.Equal(t, "localhost:8080/internal/builder:latest", imported.Name)
	t.Logf("ImportLocalImage returned: name=%s, status=%s, digest=%s", imported.Name, imported.Status, imported.Digest)

	// Step 4: Wait for the async build pipeline to complete
	waitForReady(t, mgr, ctx, imported.Name)

	// Step 5: Verify GetImage returns correct metadata
	ready, err := mgr.GetImage(ctx, imported.Name)
	require.NoError(t, err)
	require.Equal(t, StatusReady, ready.Status)
	require.Equal(t, digestStr, ready.Digest)
	require.Equal(t, []string{"/usr/local/bin/guest-agent"}, ready.Entrypoint)
	require.Equal(t, "/app", ready.WorkingDir)
	require.Contains(t, ready.Env, "PATH")
	require.NotNil(t, ready.SizeBytes)
	require.Greater(t, *ready.SizeBytes, int64(0))
	t.Logf("Image ready: entrypoint=%v, workdir=%s, size=%d", ready.Entrypoint, ready.WorkingDir, *ready.SizeBytes)

	// Step 6: Verify GetDiskPath returns path to a valid disk file
	diskPath, err := GetDiskPath(p, imported.Name, digestStr)
	require.NoError(t, err)
	diskStat, err := os.Stat(diskPath)
	require.NoError(t, err, "disk file should exist at %s", diskPath)
	require.False(t, diskStat.IsDir())
	require.Greater(t, diskStat.Size(), int64(0), "disk file should not be empty")
	t.Logf("Disk path verified: %s (%d bytes)", diskPath, diskStat.Size())
}

func TestDeleteAndRecreateDuringBuildTail(t *testing.T) {
	origFormat := DefaultImageFormat
	DefaultImageFormat = FormatCpio
	defer func() { DefaultImageFormat = origFormat }()

	dataDir := t.TempDir()
	p := paths.New(dataDir)
	mgr, err := NewManager(p, 1, nil)
	require.NoError(t, err)
	m := mgr.(*manager)

	ctx := context.Background()
	const repo = "kernel.local/test/recreate-race"
	const tag = "v1"

	testImg := createTestDockerImage(t)
	imgDigest, err := testImg.Digest()
	require.NoError(t, err)
	digestStr := imgDigest.String()

	cacheDir := p.SystemOCICache()
	layoutPath, err := layout.Write(cacheDir, empty.Index)
	require.NoError(t, err)
	require.NoError(t, layoutPath.AppendImage(testImg, layout.WithAnnotations(map[string]string{
		"org.opencontainers.image.ref.name": digestToLayoutTag(digestStr),
	})))

	digestHex := digestToLayoutTag(digestStr)
	events := make(chan StatusEvent, 2)
	m.subscribeToReady(digestHex, events)
	defer m.unsubscribeFromReady(digestHex, events)

	_, err = m.ImportLocalImage(ctx, repo, tag, digestStr)
	require.NoError(t, err)
	select {
	case event := <-events:
		require.Equal(t, StatusReady, event.Status)
	case <-time.After(30 * time.Second):
		t.Fatal("first build did not become ready")
	}
	firstMeta, err := readMetadata(p, repo, digestHex)
	require.NoError(t, err)
	require.NotEmpty(t, firstMeta.BuildID)

	slotHeld := make(chan struct{})
	releaseSlot := make(chan struct{})
	m.queue.EnqueueSuccessor(digestStr, func() {
		close(slotHeld)
		<-releaseSlot
	}, nil)
	select {
	case <-slotHeld:
	case <-time.After(5 * time.Second):
		t.Fatal("queue slot was not held")
	}

	// Delete by digest so the test does not depend on the tag symlink being
	// created after the ready notification.
	require.NoError(t, m.DeleteImage(ctx, repo+"@"+digestStr))
	recreated, err := m.ImportLocalImage(ctx, repo, tag, digestStr)
	require.NoError(t, err)
	require.Equal(t, StatusPending, recreated.Status)
	require.NotNil(t, recreated.QueuePosition)
	require.Equal(t, 1, *recreated.QueuePosition)

	currentMeta, err := readMetadata(p, repo, digestHex)
	require.NoError(t, err)
	require.NotEqual(t, firstMeta.BuildID, currentMeta.BuildID)
	require.NoError(t, m.DeleteImage(ctx, repo+"@"+digestStr))
	recreatedAgain, err := m.ImportLocalImage(ctx, repo, tag, digestStr)
	require.NoError(t, err)
	require.Equal(t, StatusPending, recreatedAgain.Status)
	require.NotNil(t, recreatedAgain.QueuePosition)
	require.Equal(t, 1, *recreatedAgain.QueuePosition)
	latestMeta, err := readMetadata(p, repo, digestHex)
	require.NoError(t, err)
	require.NotEqual(t, currentMeta.BuildID, latestMeta.BuildID)
	currentMeta = latestMeta

	waitCtx, cancelWait := context.WithCancel(ctx)
	defer cancelWait()
	waitResult := make(chan error, 1)
	go func() {
		waitResult <- m.WaitForReady(waitCtx, repo+"@"+digestStr)
	}()
	select {
	case err := <-waitResult:
		t.Fatalf("recreated image completed before successor ran: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	normalized, err := ParseNormalizedRef(repo + "@" + digestStr)
	require.NoError(t, err)
	staleRef := NewResolvedRef(normalized, digestStr)
	m.updateStatusByDigest(staleRef, StatusFailed, errors.New("stale build"), firstMeta.BuildID)
	staleResult, _, _, err := m.ociClient.extractOCIImageDetails(digestHex)
	require.NoError(t, err)
	require.ErrorIs(t, m.finalizeImage(staleRef, &pullResult{Metadata: staleResult}, 1, firstMeta.BuildID), errStaleBuild)
	currentMeta, err = readMetadata(p, repo, digestHex)
	require.NoError(t, err)
	require.Equal(t, StatusPending, currentMeta.Status)
	require.Nil(t, currentMeta.Error)

	close(releaseSlot)
	select {
	case event := <-events:
		require.Equal(t, StatusReady, event.Status)
	case <-time.After(30 * time.Second):
		t.Fatal("recreated image did not become ready")
	}
	select {
	case err := <-waitResult:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("WaitForReady did not observe the successor build")
	}
}

// waitForReady waits for an image build to complete
func waitForReady(t *testing.T, mgr Manager, ctx context.Context, imageName string) {
	for i := 0; i < 600; i++ {
		img, err := mgr.GetImage(ctx, imageName)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		if i%10 == 0 {
			t.Logf("Status: %s", img.Status)
		}

		if img.Status == StatusReady {
			return
		}

		if img.Status == StatusFailed {
			errMsg := ""
			if img.Error != nil {
				errMsg = *img.Error
			}
			t.Fatalf("Build failed: %s", errMsg)
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatal("Build did not complete within 60 seconds")
}
