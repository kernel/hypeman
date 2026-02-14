package images

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// BuildKit cache config mediatype - this is what BuildKit uses when exporting
// cache with image-manifest=true
const buildKitCacheConfigMediaType = "application/vnd.buildkit.cacheconfig.v0"

// TestUnpackLayersFailsOnBuildKitCacheMediatype verifies that hypeman's image
// unpacker fails when encountering BuildKit cache images. This reproduces the
// production issue where global cache images exported by BuildKit cannot be
// pre-pulled by hypeman because they use a non-standard config mediatype.
//
// The error occurs because:
// 1. BuildKit exports cache with --export-cache type=registry,image-manifest=true
// 2. The exported manifest uses "application/vnd.buildkit.cacheconfig.v0" as config mediatype
// 3. hypeman's unpackLayers expects "application/vnd.oci.image.config.v1+json"
// 4. umoci.UnpackRootfs fails with "config blob is not correct mediatype"
func TestUnpackLayersFailsOnBuildKitCacheMediatype(t *testing.T) {
	// Create a temp directory for the OCI layout
	cacheDir := t.TempDir()

	// Create OCI layout structure with BuildKit cache mediatype
	err := createBuildKitCacheLayout(cacheDir, "test-cache")
	require.NoError(t, err, "failed to create mock BuildKit cache layout")

	// Create OCI client and try to unpack
	client, err := newOCIClient(cacheDir)
	require.NoError(t, err)

	targetDir := t.TempDir()
	err = client.unpackLayers(context.Background(), "test-cache", targetDir)

	// This should fail with a mediatype error
	require.Error(t, err, "unpackLayers should fail on BuildKit cache mediatype")
	assert.Contains(t, err.Error(), "config", "error should mention config")

	t.Logf("Got expected error: %v", err)
}

// TestExtractMetadataSucceedsOnBuildKitCache verifies that extractOCIMetadata
// does NOT fail on BuildKit cache images - it's go-containerregistry which is
// lenient about mediatypes. The failure only happens during unpackLayers when
// umoci tries to unpack the rootfs.
func TestExtractMetadataSucceedsOnBuildKitCache(t *testing.T) {
	cacheDir := t.TempDir()

	err := createBuildKitCacheLayout(cacheDir, "test-cache")
	require.NoError(t, err)

	client, err := newOCIClient(cacheDir)
	require.NoError(t, err)

	// This succeeds because go-containerregistry doesn't validate config mediatype
	// The failure only happens in unpackLayers when umoci validates the config
	meta, err := client.extractOCIMetadata("test-cache")
	require.NoError(t, err, "extractOCIMetadata succeeds - go-containerregistry is lenient")

	// But the metadata will be empty/invalid since it's not a real OCI config
	t.Logf("Got metadata (likely empty): %+v", meta)
}

// createBuildKitCacheLayout creates an OCI layout that mimics what BuildKit
// exports when using --export-cache type=registry,image-manifest=true
//
// Layout structure:
// cacheDir/
//   ├── oci-layout          (OCI layout version marker)
//   ├── index.json          (points to manifest)
//   └── blobs/sha256/
//       ├── <manifest>      (image manifest with buildkit config mediatype)
//       ├── <config>        (buildkit cache config blob)
//       └── <layer>         (dummy layer)
func createBuildKitCacheLayout(cacheDir, layoutTag string) error {
	// Create directory structure
	blobsDir := filepath.Join(cacheDir, "blobs", "sha256")
	if err := os.MkdirAll(blobsDir, 0755); err != nil {
		return err
	}

	// 1. Create oci-layout file
	ociLayout := map[string]string{"imageLayoutVersion": "1.0.0"}
	ociLayoutBytes, _ := json.Marshal(ociLayout)
	if err := os.WriteFile(filepath.Join(cacheDir, "oci-layout"), ociLayoutBytes, 0644); err != nil {
		return err
	}

	// 2. Create a dummy layer blob (gzipped tar with a single file)
	// This is a minimal valid gzipped tar
	layerContent := []byte{
		0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, // gzip header
		0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // empty tar
	}
	layerDigest := sha256Hash(layerContent)
	if err := os.WriteFile(filepath.Join(blobsDir, layerDigest), layerContent, 0644); err != nil {
		return err
	}

	// 3. Create BuildKit cache config blob
	// This is what BuildKit puts in the config - NOT a standard OCI config
	cacheConfig := map[string]interface{}{
		"layers": []map[string]interface{}{
			{
				"blob":      "sha256:" + layerDigest,
				"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
			},
		},
	}
	configBytes, _ := json.Marshal(cacheConfig)
	configDigest := sha256Hash(configBytes)
	if err := os.WriteFile(filepath.Join(blobsDir, configDigest), configBytes, 0644); err != nil {
		return err
	}

	// 4. Create image manifest with BuildKit's cache config mediatype
	manifest := map[string]interface{}{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]interface{}{
			"mediaType": buildKitCacheConfigMediaType, // This is the problem!
			"digest":    "sha256:" + configDigest,
			"size":      len(configBytes),
		},
		"layers": []map[string]interface{}{
			{
				"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
				"digest":    "sha256:" + layerDigest,
				"size":      len(layerContent),
			},
		},
	}
	manifestBytes, _ := json.Marshal(manifest)
	manifestDigest := sha256Hash(manifestBytes)
	if err := os.WriteFile(filepath.Join(blobsDir, manifestDigest), manifestBytes, 0644); err != nil {
		return err
	}

	// 5. Create index.json pointing to the manifest with our layout tag
	index := map[string]interface{}{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]interface{}{
			{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest":    "sha256:" + manifestDigest,
				"size":      len(manifestBytes),
				"annotations": map[string]string{
					"org.opencontainers.image.ref.name": layoutTag,
				},
			},
		},
	}
	indexBytes, _ := json.Marshal(index)
	if err := os.WriteFile(filepath.Join(cacheDir, "index.json"), indexBytes, 0644); err != nil {
		return err
	}

	return nil
}

// sha256Hash computes the SHA256 hash of data and returns the hex string
func sha256Hash(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// TestConvertToOCIMediaTypePassesThroughBuildKitType verifies that the
// mediatype conversion function doesn't handle BuildKit's cache config type,
// which is the root cause of the unpack failure.
func TestConvertToOCIMediaTypePassesThroughBuildKitType(t *testing.T) {
	// Verify that BuildKit's mediatype passes through unchanged
	result := convertToOCIMediaType(buildKitCacheConfigMediaType)
	assert.Equal(t, buildKitCacheConfigMediaType, result,
		"BuildKit cache config mediatype should pass through unchanged (this is the bug)")

	// Standard Docker types should be converted
	assert.Equal(t, "application/vnd.oci.image.config.v1+json",
		convertToOCIMediaType("application/vnd.docker.container.image.v1+json"))
}

// createTestDockerImage builds a synthetic Docker image using go-containerregistry.
// This simulates what "docker build + docker save" produces without requiring Docker.
// The image contains a fake builder binary and config file, with Docker v2 mediatypes
// (matching what docker save outputs).
func createTestDockerImage(t *testing.T) v1.Image {
	t.Helper()

	// Build a gzipped tar layer with test files
	var layerBuf bytes.Buffer
	gzw := gzip.NewWriter(&layerBuf)
	tw := tar.NewWriter(gzw)

	files := []struct {
		name    string
		content string
		mode    int64
		isDir   bool
	}{
		{name: "usr/", isDir: true, mode: 0755},
		{name: "usr/local/", isDir: true, mode: 0755},
		{name: "usr/local/bin/", isDir: true, mode: 0755},
		{name: "usr/local/bin/guest-agent", content: "fake-builder-binary-v1", mode: 0755},
		{name: "etc/", isDir: true, mode: 0755},
		{name: "etc/builder.json", content: `{"version":"1.0"}`, mode: 0644},
		{name: "app/", isDir: true, mode: 0755},
	}

	for _, f := range files {
		if f.isDir {
			require.NoError(t, tw.WriteHeader(&tar.Header{
				Name:     f.name,
				Typeflag: tar.TypeDir,
				Mode:     f.mode,
			}))
		} else {
			require.NoError(t, tw.WriteHeader(&tar.Header{
				Name:     f.name,
				Size:     int64(len(f.content)),
				Typeflag: tar.TypeReg,
				Mode:     f.mode,
			}))
			_, err := tw.Write([]byte(f.content))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())

	layerBytes := layerBuf.Bytes()

	// Create layer from bytes
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(layerBytes)), nil
	})
	require.NoError(t, err)

	// Start with empty image and add our layer
	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)

	// Set config (entrypoint, env, workdir) - matches what a real builder image would have
	img, err = mutate.Config(img, v1.Config{
		Entrypoint: []string{"/usr/local/bin/guest-agent"},
		Env:        []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		WorkingDir: "/app",
	})
	require.NoError(t, err)

	return img
}

// TestDockerSaveTarballToOCILayoutRoundtrip tests the exact pipeline used by
// buildBuilderFromDockerfile: docker save tarball → load via go-containerregistry
// → write to OCI layout cache → verify existsInLayout + extractMetadata + unpackLayers.
//
// This simulates:
// 1. docker build → docker save (we use go-containerregistry to create the tarball)
// 2. tarball.ImageFromPath (load the docker save output)
// 3. layout.AppendImage with digest annotation (write to OCI cache)
// 4. existsInLayout (cache hit detection)
// 5. extractOCIMetadata (read config from cache)
// 6. unpackLayers (unpack rootfs from cache)
func TestDockerSaveTarballToOCILayoutRoundtrip(t *testing.T) {
	// Step 1: Create a synthetic Docker image (simulates docker build output)
	img := createTestDockerImage(t)

	// Step 2: Save as docker save tarball (simulates docker save)
	tarPath := filepath.Join(t.TempDir(), "image.tar")
	tag, err := name.NewTag("localhost:5000/test/builder:latest")
	require.NoError(t, err)
	require.NoError(t, tarball.WriteToFile(tarPath, tag, img))

	// Step 3: Load from tarball (this is what buildBuilderFromDockerfile does)
	loadedImg, err := tarball.ImageFromPath(tarPath, nil)
	require.NoError(t, err)

	// Get digest (used as OCI layout tag)
	imgDigest, err := loadedImg.Digest()
	require.NoError(t, err)
	digestStr := imgDigest.String() // "sha256:abc123..."
	layoutTag := digestToLayoutTag(digestStr)
	t.Logf("Image digest: %s, layoutTag: %s", digestStr, layoutTag)

	// Step 4: Write to OCI layout (simulates the layout.AppendImage in buildBuilderFromDockerfile)
	cacheDir := t.TempDir()
	path, err := layout.Write(cacheDir, empty.Index)
	require.NoError(t, err)

	err = path.AppendImage(loadedImg, layout.WithAnnotations(map[string]string{
		"org.opencontainers.image.ref.name": layoutTag,
	}))
	require.NoError(t, err)

	// Step 5: Create OCI client and verify existsInLayout (cache hit detection)
	client, err := newOCIClient(cacheDir)
	require.NoError(t, err)
	assert.True(t, client.existsInLayout(layoutTag), "image should exist in layout after AppendImage")

	// Step 6: Verify extractOCIMetadata reads correct config
	meta, err := client.extractOCIMetadata(layoutTag)
	require.NoError(t, err)
	assert.Equal(t, []string{"/usr/local/bin/guest-agent"}, meta.Entrypoint)
	assert.Equal(t, "/app", meta.WorkingDir)
	assert.Contains(t, meta.Env, "PATH")

	// Step 7: Verify unpackLayers produces correct rootfs
	// umoci's UnpackRootfs extracts directly into the target directory
	unpackDir := filepath.Join(t.TempDir(), "unpack")
	err = client.unpackLayers(context.Background(), layoutTag, unpackDir)
	require.NoError(t, err)

	// Verify expected files exist in unpacked rootfs
	agentPath := filepath.Join(unpackDir, "usr", "local", "bin", "guest-agent")
	agentContent, err := os.ReadFile(agentPath)
	require.NoError(t, err, "guest-agent binary should exist in unpacked rootfs")
	assert.Equal(t, "fake-builder-binary-v1", string(agentContent))

	builderJSON := filepath.Join(unpackDir, "etc", "builder.json")
	jsonContent, err := os.ReadFile(builderJSON)
	require.NoError(t, err, "builder.json should exist in unpacked rootfs")
	assert.Equal(t, `{"version":"1.0"}`, string(jsonContent))

	appDir := filepath.Join(unpackDir, "app")
	stat, err := os.Stat(appDir)
	require.NoError(t, err, "/app directory should exist")
	assert.True(t, stat.IsDir())

	t.Log("Full roundtrip verified: docker save tarball → OCI layout → existsInLayout → extractMetadata → unpackLayers")
}

// TestDockerSaveToOCILayoutCacheHit verifies that pullAndExport correctly
// detects a cache hit when the image has already been written to OCI layout
// (via AppendImage), skipping the remote pull entirely. This is the exact
// flow when buildBuilderFromDockerfile writes to cache and then ImportLocalImage
// triggers buildImage → pullAndExport.
func TestDockerSaveToOCILayoutCacheHit(t *testing.T) {
	// Create synthetic image and write to OCI layout
	img := createTestDockerImage(t)

	imgDigest, err := img.Digest()
	require.NoError(t, err)
	digestStr := imgDigest.String()
	layoutTag := digestToLayoutTag(digestStr)

	cacheDir := t.TempDir()
	path, err := layout.Write(cacheDir, empty.Index)
	require.NoError(t, err)

	err = path.AppendImage(img, layout.WithAnnotations(map[string]string{
		"org.opencontainers.image.ref.name": layoutTag,
	}))
	require.NoError(t, err)

	// Create OCI client pointing at same cache dir
	client, err := newOCIClient(cacheDir)
	require.NoError(t, err)

	// Call pullAndExport with a bogus imageRef — since the digest is already cached,
	// it should NOT attempt a remote pull and should succeed from cache alone
	exportDir := filepath.Join(t.TempDir(), "export")
	result, err := client.pullAndExport(
		context.Background(),
		"localhost:9999/nonexistent/image:v1", // would fail if it tried to pull
		digestStr,
		exportDir,
	)
	require.NoError(t, err, "pullAndExport should succeed from cache without remote pull")
	require.NotNil(t, result)

	// Verify metadata was extracted
	assert.Equal(t, []string{"/usr/local/bin/guest-agent"}, result.Metadata.Entrypoint)
	assert.Equal(t, "/app", result.Metadata.WorkingDir)
	assert.Equal(t, digestStr, result.Digest)

	// Verify rootfs was unpacked (umoci extracts directly into exportDir)
	agentPath := filepath.Join(exportDir, "usr", "local", "bin", "guest-agent")
	content, err := os.ReadFile(agentPath)
	require.NoError(t, err)
	assert.Equal(t, "fake-builder-binary-v1", string(content))

	t.Log("Cache hit verified: pullAndExport skipped remote pull and used OCI layout cache")
}
