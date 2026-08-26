package images

import (
	"os"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testContentDigest = "abababababababababababababababababababababababababababababababab"
	testConfigDigest  = "sha256:5c3b0e9f2a8d4e71c6f9a0b3d2e5c8f7a1b4d6e9c2f5a8b1d4e7c0f3a6b9d2e5"
	testLayerDigestA  = "sha256:2d35eb2672e34f00a51cb6ab8a4a4d0e07e6c35fa20b7dc7b8a2b5ef7c86329a"
	testLayerDigestB  = "sha256:44cf07bba1681a35a0a2ce4f11fbe16ad0b8e57f69965f1970e51bec2878aa70"
	testDiffIDA       = "sha256:706db5f26d91b7d8f9c6a6f3f2d34c4be1b04e37f4f32a0e29bb5345bb6bb26e"
)

func TestParseContentRef(t *testing.T) {
	hex := "abababababababababababababababababababababababababababababababab"

	t.Run("full digest", func(t *testing.T) {
		ref, err := parseContentRef("sha256:" + hex)
		require.NoError(t, err)
		assert.Equal(t, hex, ref.Hex())
		assert.Equal(t, "sha256:"+hex, ref.Digest())
	})

	t.Run("bare hex gets sha256 prefix", func(t *testing.T) {
		ref, err := parseContentRef(hex)
		require.NoError(t, err)
		assert.Equal(t, hex, ref.Hex())
	})

	for _, invalid := range []string{
		"",
		"sha256:abc",     // truncated hex
		"sha256:" + "zz", // non-hex characters
		"md5:" + hex,     // unsupported algorithm
		":abc123",        // malformed prefix
	} {
		_, err := parseContentRef(invalid)
		assert.Error(t, err, "parseContentRef(%q) should fail", invalid)
	}
}

func TestLayerDescriptorValidate(t *testing.T) {
	valid := layerDescriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest:    testLayerDigestA,
		Size:      1024,
		DiffID:    testDiffIDA,
	}
	assert.NoError(t, valid.validate())

	invalidDigest := valid
	invalidDigest.Digest = "not-a-digest"
	assert.Error(t, invalidDigest.validate())

	negativeSize := valid
	negativeSize.Size = -1
	assert.Error(t, negativeSize.validate())

	invalidDiffID := valid
	invalidDiffID.DiffID = "sha256:nothex"
	assert.Error(t, invalidDiffID.validate())

	// DiffID is optional: images may omit it.
	noDiffID := valid
	noDiffID.DiffID = ""
	assert.NoError(t, noDiffID.validate())
}

func testConfigDescriptor() configDescriptor {
	return configDescriptor{
		MediaType: "application/vnd.oci.image.config.v1+json",
		Digest:    testConfigDigest,
		Size:      512,
	}
}

func TestConfigDescriptorValidate(t *testing.T) {
	assert.NoError(t, testConfigDescriptor().validate())

	invalidDigest := testConfigDescriptor()
	invalidDigest.Digest = "not-a-digest"
	assert.Error(t, invalidDigest.validate())

	negativeSize := testConfigDescriptor()
	negativeSize.Size = -1
	assert.Error(t, negativeSize.validate())
}

func testLayerRecord() *imageLayers {
	return newImageLayers(testConfigDescriptor(), []layerDescriptor{
		{
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			Digest:    testLayerDigestA,
			Size:      1024,
			DiffID:    testDiffIDA,
		},
		{
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			Digest:    testLayerDigestB,
			Size:      2048,
		},
	})
}

func TestWriteAndReadImageLayersRoundtrip(t *testing.T) {
	p := paths.New(t.TempDir())
	repository := "docker.io/library/alpine"
	ref, err := parseContentRef("sha256:" + testContentDigest)
	require.NoError(t, err)

	record := testLayerRecord()
	require.NoError(t, writeImageLayers(p, repository, ref, record))
	require.FileExists(t, p.ImageContentLayers(ref.Hex()))

	got, err := readImageLayers(p, repository, ref)
	require.NoError(t, err)
	assert.Equal(t, layersSchemaVersion, got.SchemaVersion)
	assert.Equal(t, testConfigDescriptor(), got.Config)
	require.Len(t, got.Layers, 2)
	// Order is significant: layers must come back in manifest order.
	assert.Equal(t, testLayerDigestA, got.Layers[0].Digest)
	assert.Equal(t, int64(1024), got.Layers[0].Size)
	assert.Equal(t, testDiffIDA, got.Layers[0].DiffID)
	assert.Equal(t, testLayerDigestB, got.Layers[1].Digest)
	assert.Equal(t, int64(2048), got.Layers[1].Size)
}

func TestReadImageLayersNotFound(t *testing.T) {
	p := paths.New(t.TempDir())
	ref, err := parseContentRef("sha256:" + testContentDigest)
	require.NoError(t, err)

	_, err = readImageLayers(p, "docker.io/library/alpine", ref)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestWriteImageLayersRejectsInvalidRecord(t *testing.T) {
	p := paths.New(t.TempDir())
	ref, err := parseContentRef("sha256:" + testContentDigest)
	require.NoError(t, err)

	record := testLayerRecord()
	record.Layers[1].Digest = "bogus"
	err = writeImageLayers(p, "docker.io/library/alpine", ref, record)
	require.Error(t, err)

	invalidConfig := testLayerRecord()
	invalidConfig.Config.Digest = "bogus"
	err = writeImageLayers(p, "docker.io/library/alpine", ref, invalidConfig)
	require.Error(t, err)

	_, statErr := os.Stat(p.ImageContentLayers(ref.Hex()))
	require.ErrorIs(t, statErr, os.ErrNotExist, "invalid record must not leave a file behind")
}

func TestWriteImageLayersFollowsActiveLayout(t *testing.T) {
	t.Run("complete legacy image keeps layers in legacy directory", func(t *testing.T) {
		p := paths.New(t.TempDir())
		repository := "docker.io/library/alpine"
		ref, err := parseContentRef("sha256:" + testContentDigest)
		require.NoError(t, err)

		seedLegacyReadyImage(t, p, repository, ref.Hex())

		require.NoError(t, writeImageLayers(p, repository, ref, testLayerRecord()))
		require.FileExists(t, p.ImageLayers(repository, ref.Hex()))
		_, err = os.Stat(p.ImageContentLayers(ref.Hex()))
		require.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("ready content wins over legacy", func(t *testing.T) {
		p := paths.New(t.TempDir())
		repository := "docker.io/library/alpine"
		ref, err := parseContentRef("sha256:" + testContentDigest)
		require.NoError(t, err)

		seedLegacyReadyImage(t, p, repository, ref.Hex())
		require.NoError(t, writeMetadataFile(p.ImageContentMetadata(ref.Hex()), &imageMetadata{
			Name:   repository + ":latest",
			Digest: ref.Digest(),
			Status: StatusReady,
		}))
		require.NoError(t, os.WriteFile(p.ImageContentPath(ref.Hex()), []byte("content rootfs"), 0o644))

		require.NoError(t, writeImageLayers(p, repository, ref, testLayerRecord()))
		require.FileExists(t, p.ImageContentLayers(ref.Hex()))
	})
}

func TestPromoteImageToContentCarriesLayerManifest(t *testing.T) {
	p := paths.New(t.TempDir())
	repository := "docker.io/library/alpine"
	ref, err := parseContentRef("sha256:" + testContentDigest)
	require.NoError(t, err)

	meta := &imageMetadata{
		Name:      repository + ":latest",
		Digest:    ref.Digest(),
		Status:    StatusReady,
		SizeBytes: int64(len("legacy rootfs")),
	}
	seedLegacyReadyImage(t, p, repository, ref.Hex())
	require.NoError(t, writeMetadataFile(p.ImageMetadata(repository, ref.Hex()), meta))
	require.NoError(t, writeImageLayers(p, repository, ref, testLayerRecord()))

	require.NoError(t, promoteImageToContent(p, repository, ref.Hex(), meta))

	got, err := readContentMetadata(p, ref.Hex())
	require.NoError(t, err)
	require.Equal(t, StatusReady, got.Status)

	layers, err := readImageLayers(p, repository, ref)
	require.NoError(t, err)
	require.Len(t, layers.Layers, 2)
	assert.Equal(t, testLayerDigestA, layers.Layers[0].Digest)

	// The legacy tree is removed after promotion; the layer list must have
	// survived in the shared content directory.
	_, err = os.Stat(p.ImageDigestDir(repository, ref.Hex()))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// seedLegacyReadyImage writes a complete legacy-layout image (metadata + disk).
func seedLegacyReadyImage(t *testing.T, p *paths.Paths, repository, digestHex string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(p.ImageDigestDir(repository, digestHex), 0o755))
	require.NoError(t, writeMetadataFile(p.ImageMetadata(repository, digestHex), &imageMetadata{
		Name:      repository + ":latest",
		Digest:    "sha256:" + digestHex,
		Status:    StatusReady,
		SizeBytes: int64(len("legacy rootfs")),
	}))
	require.NoError(t, os.WriteFile(p.ImageDigestPath(repository, digestHex), []byte("legacy rootfs"), 0o644))
}
