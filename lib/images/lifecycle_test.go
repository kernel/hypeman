package images

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gcr "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

// writeSharedLayout writes several images into one OCI layout cache, each
// annotated with its own digest tag.
func writeSharedLayout(t *testing.T, p *paths.Paths, imgs ...gcr.Image) []string {
	t.Helper()

	layoutPath, err := layout.Write(p.SystemOCICache(), empty.Index)
	require.NoError(t, err)

	digests := make([]string, 0, len(imgs))
	for _, img := range imgs {
		digest, err := img.Digest()
		require.NoError(t, err)
		require.NoError(t, layoutPath.AppendImage(img, layout.WithAnnotations(map[string]string{
			"org.opencontainers.image.ref.name": digestToLayoutTag(digest.String()),
		})))
		digests = append(digests, digest.String())
	}
	return digests
}

func layerHexes(t *testing.T, p *paths.Paths) map[string]struct{} {
	t.Helper()
	entries, err := os.ReadDir(p.ImageLayersDir())
	require.NoError(t, err)
	hexes := make(map[string]struct{})
	for _, entry := range entries {
		if entry.IsDir() {
			hexes[entry.Name()] = struct{}{}
		}
	}
	return hexes
}

// TestSharedLayersMaterializeOnceAndEvictWithReferences is the end-to-end
// lifecycle: two images share a base layer, the shared artifact is created
// once, survives the deletion of one image, and is evicted only when its last
// reference is gone.
func TestSharedLayersMaterializeOnceAndEvictWithReferences(t *testing.T) {
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs not available")
	}
	dataDir := t.TempDir()
	p := paths.New(dataDir)
	mgr, err := NewManager(p, 1, nil)
	require.NoError(t, err)
	m := mgr.(*manager)
	m.layerEvictionGrace = 0

	base := syntheticLayer(t, "base.txt", "shared base content")
	topA := syntheticLayer(t, "a.txt", "app A payload")
	topB := syntheticLayer(t, "b.txt", "app B payload")

	imgA, err := mutate.AppendLayers(empty.Image, base, topA)
	require.NoError(t, err)
	imgB, err := mutate.AppendLayers(empty.Image, base, topB)
	require.NoError(t, err)

	digests := writeSharedLayout(t, p, imgA, imgB)
	digestA, digestB := digests[0], digests[1]

	baseManifest, err := imgA.Manifest()
	require.NoError(t, err)
	baseHex := baseManifest.Layers[0].Digest.Hex
	topAHex := baseManifest.Layers[1].Digest.Hex
	topBManifest, err := imgB.Manifest()
	require.NoError(t, err)
	topBHex := topBManifest.Layers[1].Digest.Hex

	ctx := context.Background()
	const repoA = "kernel.local/apps/app-a"
	const repoB = "kernel.local/apps/app-b"

	eventsA := make(chan StatusEvent, 2)
	m.subscribeToReady(digestToLayoutTag(digestA), eventsA)
	defer m.unsubscribeFromReady(digestToLayoutTag(digestA), eventsA)
	_, err = m.ImportLocalImage(ctx, repoA, "v1", digestA)
	require.NoError(t, err)
	select {
	case event := <-eventsA:
		require.Equal(t, StatusReady, event.Status)
	case <-time.After(30 * time.Second):
		t.Fatal("image A did not become ready")
	}

	eventsB := make(chan StatusEvent, 2)
	m.subscribeToReady(digestToLayoutTag(digestB), eventsB)
	defer m.unsubscribeFromReady(digestToLayoutTag(digestB), eventsB)
	_, err = m.ImportLocalImage(ctx, repoB, "v1", digestB)
	require.NoError(t, err)
	select {
	case event := <-eventsB:
		require.Equal(t, StatusReady, event.Status)
	case <-time.After(30 * time.Second):
		t.Fatal("image B did not become ready")
	}

	// The shared base layer materialized exactly once, alongside the two tops.
	hexes := layerHexes(t, p)
	require.Len(t, hexes, 3)
	require.Contains(t, hexes, baseHex)
	require.Contains(t, hexes, topAHex)
	require.Contains(t, hexes, topBHex)

	// Deleting image A evicts only its unique layer; the shared base survives.
	require.NoError(t, m.DeleteImage(ctx, repoA+"@"+digestA))
	hexes = layerHexes(t, p)
	require.Len(t, hexes, 2)
	require.Contains(t, hexes, baseHex, "shared base must survive while referenced")
	require.Contains(t, hexes, topBHex)
	require.NotContains(t, hexes, topAHex)

	// Deleting image B removes the last references: everything is evicted.
	require.NoError(t, m.DeleteImage(ctx, repoB+"@"+digestB))
	hexes = layerHexes(t, p)
	require.Empty(t, hexes, "unreferenced layer artifacts must be evicted")
}

func newLifecycleTestManager(p *paths.Paths) *manager {
	return &manager{
		paths:             p,
		inflightPulls:     make(map[string]*inflightImagePull),
		inflightLayerRefs: make(map[string]int),
	}
}

func TestTotalImageBytesIncludesLayerArtifacts(t *testing.T) {
	p := paths.New(t.TempDir())
	m := newLifecycleTestManager(p)

	digestHex := "cd01cd01cd01cd01cd01cd01cd01cd01cd01cd01cd01cd01cd01cd01cd01cd01"
	require.NoError(t, os.MkdirAll(p.ImageLayerDir(digestHex), 0o755))
	payload := make([]byte, 4096)
	require.NoError(t, os.WriteFile(p.ImageLayerArtifactForFormat(digestHex, string(DefaultImageFormat)), payload, 0o644))

	readyBytes, cacheBytes, err := m.getDiskUsageTotals()
	require.NoError(t, err)
	require.GreaterOrEqual(t, cacheBytes, int64(len(payload)))

	totalBytes, err := m.TotalImageBytes(context.Background())
	require.NoError(t, err)
	require.Equal(t, readyBytes+cacheBytes, totalBytes)
}

func TestCleanStaleImageTempDirsRemovesOnlyOldDirectories(t *testing.T) {
	p := paths.New(t.TempDir())
	m := newLifecycleTestManager(p)
	m.layerEvictionGrace = time.Hour

	layersDir := p.ImageLayersDir()
	staleDir := filepath.Join(layersDir, "ab12", ".unpack-stale")
	freshDir := filepath.Join(layersDir, "cd34", ".unpack-fresh")
	require.NoError(t, os.MkdirAll(staleDir, 0o755))
	require.NoError(t, os.MkdirAll(freshDir, 0o755))
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(staleDir, old, old))

	m.cleanStaleImageTempDirs()

	_, err := os.Stat(staleDir)
	require.True(t, os.IsNotExist(err), "stale temp dir must be removed")
	_, err = os.Stat(freshDir)
	require.NoError(t, err, "fresh temp dir must survive cleanup")
}

func TestEvictionKeepsReferencedAndFreshArtifacts(t *testing.T) {
	p := paths.New(t.TempDir())
	m := newLifecycleTestManager(p)
	m.layerEvictionGrace = time.Hour

	referencedHex := "ef01ef01ef01ef01ef01ef01ef01ef01ef01ef01ef01ef01ef01ef01ef01ef01"
	orphanFreshHex := "ab23ab23ab23ab23ab23ab23ab23ab23ab23ab23ab23ab23ab23ab23ab23ab23"

	// A manifest model referencing one layer protects it regardless of age.
	model := &imageManifestModel{
		SchemaVersion: manifestModelSchemaVersion,
		Digest:        "sha256:" + referencedHex,
		RootFSType:    "layers",
		Config: manifestConfigRef{
			Digest:    "sha256:" + strings.Repeat("c", 64),
			MediaType: "application/vnd.oci.image.config.v1+json",
			DiffIDs:   []string{"sha256:" + referencedHex},
		},
		Layers: []layerDescriptor{{Digest: "sha256:" + referencedHex, DiffID: "sha256:" + referencedHex}},
	}
	require.NoError(t, writeManifestModel(p, referencedHex, model))
	require.NoError(t, os.MkdirAll(p.ImageLayerDir(referencedHex), 0o755))
	require.NoError(t, os.WriteFile(p.ImageLayerArtifactForFormat(referencedHex, string(DefaultImageFormat)), []byte("kept"), 0o644))

	// An unreferenced but fresh artifact is protected by the grace period.
	require.NoError(t, os.MkdirAll(p.ImageLayerDir(orphanFreshHex), 0o755))
	require.NoError(t, os.WriteFile(p.ImageLayerArtifactForFormat(orphanFreshHex, string(DefaultImageFormat)), []byte("fresh"), 0o644))

	m.reconcileLayerStore()

	hexes := layerHexes(t, p)
	require.Contains(t, hexes, referencedHex)
	require.Contains(t, hexes, orphanFreshHex)
}
