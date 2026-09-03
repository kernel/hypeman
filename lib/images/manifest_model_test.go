package images

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	gcr "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

// syntheticLayer builds a gzipped tar layer containing one file.
func syntheticLayer(t *testing.T, name, content string) gcr.Layer {
	t.Helper()
	return specLayer(t, []tarEntrySpec{{name: name, content: content, mode: 0644}})
}

// writeSyntheticLayout writes img into a temp-dir-backed paths root and
// returns a client for its cache plus the image's layout tag.
func writeSyntheticLayout(t *testing.T, img gcr.Image) (*ociClient, string) {
	t.Helper()

	p := paths.New(t.TempDir())
	writeLayerTestLayout(t, p, img)

	client, err := newOCIClient(p.SystemOCICache())
	require.NoError(t, err)
	digest, err := img.Digest()
	require.NoError(t, err)
	return client, digestToLayoutTag(digest.String())
}

func TestExtractManifestModel(t *testing.T) {
	base := syntheticLayer(t, "base.txt", "base layer content")
	top := syntheticLayer(t, "top.txt", "top layer content")
	img, err := mutate.AppendLayers(empty.Image, base, top)
	require.NoError(t, err)
	img = mutate.MediaType(img, types.OCIManifestSchema1)

	client, layoutTag := writeSyntheticLayout(t, img)

	bundle, err := client.extractOCIImageBundle(layoutTag)
	require.NoError(t, err)
	model := bundle.Model

	manifest, err := img.Manifest()
	require.NoError(t, err)
	configFile, err := img.ConfigFile()
	require.NoError(t, err)
	configDigest, err := img.ConfigName()
	require.NoError(t, err)

	require.Equal(t, "sha256:"+layoutTag, model.Digest)
	require.Equal(t, manifest.MediaType, types.MediaType(model.MediaType))
	require.Equal(t, configDigest.String(), model.Config.Digest)
	require.Len(t, model.Layers, 2)
	require.Len(t, model.Config.DiffIDs, 2)

	// Layer order and pairing must match the manifest and config diff ids.
	for i, desc := range model.Layers {
		require.Equal(t, manifest.Layers[i].Digest.String(), desc.Digest)
		require.Equal(t, manifest.Layers[i].Size, desc.Size)
		require.Equal(t, configFile.RootFS.DiffIDs[i].String(), desc.DiffID)
		require.Equal(t, model.Config.DiffIDs[i], desc.DiffID)
	}

	// Digests are content addresses, so identical layer content dedupes.
	require.NotEqual(t, model.Layers[0].Digest, model.Layers[1].Digest)
}

func TestExtractManifestModelPlatform(t *testing.T) {
	layer := syntheticLayer(t, "file.txt", "content")
	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	cfgFile, err := img.ConfigFile()
	require.NoError(t, err)
	cfgFile = cfgFile.DeepCopy()
	cfgFile.OS = "linux"
	cfgFile.Architecture = "amd64"
	img, err = mutate.ConfigFile(img, cfgFile)
	require.NoError(t, err)

	client, layoutTag := writeSyntheticLayout(t, img)

	bundle, err := client.extractOCIImageBundle(layoutTag)
	require.NoError(t, err)
	model := bundle.Model
	require.Equal(t, "linux/amd64", model.Platform)
}

func TestManifestModelWriteReadRoundtrip(t *testing.T) {
	p := paths.New(t.TempDir())
	digestHex := "ab01ab01ab01ab01ab01ab01ab01ab01ab01ab01ab01ab01ab01ab01ab01ab01"

	model := &imageManifestModel{
		SchemaVersion: manifestModelSchemaVersion,
		Digest:        "sha256:" + digestHex,
		RootFSType:    "layers",
		Platform:      "linux/amd64",
		Config: manifestConfigRef{
			Digest:  "sha256:" + strings.Repeat("c", 64),
			DiffIDs: []string{"sha256:" + strings.Repeat("d", 64), "sha256:" + strings.Repeat("e", 64)},
		},
		Layers: []layerDescriptor{
			{Digest: "sha256:" + strings.Repeat("f", 64), Size: 10, DiffID: "sha256:" + strings.Repeat("d", 64)},
			{Digest: "sha256:" + strings.Repeat("0", 64), Size: 20, DiffID: "sha256:" + strings.Repeat("e", 64)},
		},
	}
	require.NoError(t, writeManifestModel(p, digestHex, model))

	read, err := readManifestModel(p, digestHex)
	require.NoError(t, err)
	require.Equal(t, model, read)

	// No leftover temp files from the atomic write.
	entries, err := os.ReadDir(p.ImageContentDir(digestHex))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "manifest.json", entries[0].Name())
}

func TestReadManifestModelRejectsInvalidSchema(t *testing.T) {
	p := paths.New(t.TempDir())
	digestHex := strings.Repeat("a", 64)
	require.NoError(t, os.MkdirAll(p.ImageContentDir(digestHex), 0755))
	require.NoError(t, os.WriteFile(p.ImageContentManifestModel(digestHex),
		[]byte(`{"schema_version":1,"digest":"sha256:`+digestHex+`","rootfs_type":"layers"}`), 0644))
	_, err := readManifestModel(p, digestHex)
	require.ErrorContains(t, err, "config digest is empty")
}

func TestValidateManifestModelRejectsNonLayeredRootFS(t *testing.T) {
	model := &imageManifestModel{
		SchemaVersion: manifestModelSchemaVersion,
		Digest:        "sha256:" + strings.Repeat("ab", 32),
		RootFSType:    "",
		Config:        manifestConfigRef{Digest: "sha256:" + strings.Repeat("cd", 32)},
	}
	err := validateManifestModel(model.Digest, model)
	require.ErrorContains(t, err, "rootfs type")

	model.RootFSType = "flattened"
	err = validateManifestModel(model.Digest, model)
	require.ErrorContains(t, err, "rootfs type")
}

func TestReadManifestModelMissing(t *testing.T) {
	p := paths.New(t.TempDir())
	model, err := readManifestModel(p, "deadbeef")
	require.NoError(t, err)
	require.Nil(t, model, "missing model must read as nil for pre-model images")
}

func TestManifestModelBlobReferences(t *testing.T) {
	model := &imageManifestModel{
		Config: manifestConfigRef{Digest: "sha256:cfg"},
		Layers: []layerDescriptor{
			{Digest: "sha256:layer0"},
			{Digest: "sha256:layer1"},
		},
	}
	require.Equal(t, []string{"sha256:cfg", "sha256:layer0", "sha256:layer1"}, model.blobReferences())
}

func TestImportLocalImagePersistsManifestModel(t *testing.T) {
	dataDir := t.TempDir()
	p := paths.New(dataDir)
	mgr, err := NewManager(p, 1, nil)
	require.NoError(t, err)
	m := mgr.(*manager)

	ctx := context.Background()
	const repo = "kernel.local/test/manifest-model"
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
		t.Fatal("build did not become ready")
	}

	model, err := readManifestModel(p, digestHex)
	require.NoError(t, err)
	require.NotNil(t, model, "ready image must persist a manifest model")
	require.Equal(t, "sha256:"+digestHex, model.Digest)

	configFile, err := testImg.ConfigFile()
	require.NoError(t, err)
	manifest, err := testImg.Manifest()
	require.NoError(t, err)
	require.Len(t, model.Layers, len(manifest.Layers))
	for i, desc := range model.Layers {
		require.Equal(t, manifest.Layers[i].Digest.String(), desc.Digest)
		require.Equal(t, configFile.RootFS.DiffIDs[i].String(), desc.DiffID)
	}
	require.Equal(t, StatusReady, mustReadContentStatus(t, p, digestHex))
}

func mustReadContentStatus(t *testing.T, p *paths.Paths, digestHex string) string {
	t.Helper()
	meta, err := readContentMetadata(p, digestHex)
	require.NoError(t, err)
	return meta.Status
}
