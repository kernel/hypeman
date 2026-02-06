package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
