package ocicachegc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// layoutBuilder incrementally writes OCI layout blobs and assembles an
// index.json so tests can set up realistic cache contents.
type layoutBuilder struct {
	t        *testing.T
	paths    *paths.Paths
	blobsDir string
	cacheDir string
	entries  []indexEntry
}

type indexEntry struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int               `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

func newLayoutBuilder(t *testing.T, dataDir string) *layoutBuilder {
	t.Helper()
	p := paths.New(dataDir)
	blobsDir := p.OCICacheBlobDir()
	require.NoError(t, os.MkdirAll(blobsDir, 0o755))
	return &layoutBuilder{
		t:        t,
		paths:    p,
		blobsDir: blobsDir,
		cacheDir: p.SystemOCICache(),
	}
}

// writeBlob stores content at blobs/sha256/<digest> and returns the
// digest string in canonical "sha256:<hex>" form.
func (b *layoutBuilder) writeBlob(content []byte) string {
	b.t.Helper()
	sum := sha256.Sum256(content)
	hexDigest := hex.EncodeToString(sum[:])
	require.NoError(b.t, os.WriteFile(filepath.Join(b.blobsDir, hexDigest), content, 0o644))
	return "sha256:" + hexDigest
}

// writeOrphan stores a blob that won't be referenced by any manifest.
// Returns the filename (hex digest) so tests can assert on it.
func (b *layoutBuilder) writeOrphan(content []byte, mtime time.Time) string {
	b.t.Helper()
	sum := sha256.Sum256(content)
	hexDigest := hex.EncodeToString(sum[:])
	path := filepath.Join(b.blobsDir, hexDigest)
	require.NoError(b.t, os.WriteFile(path, content, 0o644))
	if !mtime.IsZero() {
		require.NoError(b.t, os.Chtimes(path, mtime, mtime))
	}
	return hexDigest
}

// addImage appends an image manifest to the layout. Config and layer
// blobs are written first, then the manifest itself, then an index entry
// is recorded so writeIndex will include it.
func (b *layoutBuilder) addImage(tag string, configContent []byte, layerContents [][]byte) {
	b.t.Helper()

	configDigest := b.writeBlob(configContent)

	type desc struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int    `json:"size"`
	}
	layers := make([]desc, len(layerContents))
	for i, content := range layerContents {
		digest := b.writeBlob(content)
		layers[i] = desc{
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			Digest:    digest,
			Size:      len(content),
		}
	}

	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": desc{
			MediaType: "application/vnd.oci.image.config.v1+json",
			Digest:    configDigest,
			Size:      len(configContent),
		},
		"layers": layers,
	}
	manifestBytes, err := json.Marshal(manifest)
	require.NoError(b.t, err)
	manifestDigest := b.writeBlob(manifestBytes)

	b.entries = append(b.entries, indexEntry{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    manifestDigest,
		Size:      len(manifestBytes),
		Annotations: map[string]string{
			"org.opencontainers.image.ref.name": tag,
		},
	})
}

func (b *layoutBuilder) writeIndex() {
	b.t.Helper()
	index := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     b.entries,
	}
	data, err := json.Marshal(index)
	require.NoError(b.t, err)
	require.NoError(b.t, os.WriteFile(b.paths.OCICacheIndex(), data, 0o644))

	layout := map[string]string{"imageLayoutVersion": "1.0.0"}
	layoutBytes, err := json.Marshal(layout)
	require.NoError(b.t, err)
	require.NoError(b.t, os.WriteFile(b.paths.OCICacheLayout(), layoutBytes, 0o644))
}

func newCollectorForTest(t *testing.T, dataDir string, minBlobAge time.Duration, now time.Time) *Collector {
	t.Helper()
	c, err := NewCollector(paths.New(dataDir), time.Hour, minBlobAge, nil, nil)
	require.NoError(t, err)
	c.now = func() time.Time { return now }
	return c
}

func TestCollectNoCacheDirIsNoop(t *testing.T) {
	dataDir := t.TempDir()
	c := newCollectorForTest(t, dataDir, time.Hour, time.Now())

	stats, err := c.Collect(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, stats.ScannedBlobs)
	assert.Equal(t, 0, stats.DeletedBlobs)
}

func TestCollectKeepsLiveBlobs(t *testing.T) {
	dataDir := t.TempDir()
	b := newLayoutBuilder(t, dataDir)
	b.addImage("img-a", []byte(`{"config":"a"}`), [][]byte{[]byte("layer-a-1"), []byte("layer-a-2")})
	b.addImage("img-b", []byte(`{"config":"b"}`), [][]byte{[]byte("layer-b-1")})
	b.writeIndex()

	now := time.Now()
	c := newCollectorForTest(t, dataDir, time.Minute, now)

	stats, err := c.Collect(context.Background())
	require.NoError(t, err)

	// 2 manifests + 2 configs + 3 layers = 7 live blobs, all present.
	assert.Equal(t, 7, stats.LiveBlobs)
	assert.Equal(t, 7, stats.ScannedBlobs)
	assert.Equal(t, 0, stats.DeletedBlobs)

	// All blob files should still exist.
	entries, err := os.ReadDir(b.blobsDir)
	require.NoError(t, err)
	assert.Len(t, entries, 7)
}

func TestCollectDeletesOrphans(t *testing.T) {
	dataDir := t.TempDir()
	b := newLayoutBuilder(t, dataDir)
	b.addImage("img-a", []byte(`{"config":"a"}`), [][]byte{[]byte("layer-a")})
	b.writeIndex()

	now := time.Now()
	// Old orphan: well outside the grace period.
	orphan := b.writeOrphan([]byte("orphaned-layer"), now.Add(-2*time.Hour))

	c := newCollectorForTest(t, dataDir, time.Minute, now)
	stats, err := c.Collect(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 3, stats.LiveBlobs)
	assert.Equal(t, 4, stats.ScannedBlobs)
	assert.Equal(t, 1, stats.DeletedBlobs)
	assert.Equal(t, int64(len("orphaned-layer")), stats.DeletedBytes)

	_, err = os.Stat(filepath.Join(b.blobsDir, orphan))
	assert.True(t, os.IsNotExist(err), "orphan should be deleted")
}

func TestCollectSkipsRecentBlobs(t *testing.T) {
	dataDir := t.TempDir()
	b := newLayoutBuilder(t, dataDir)
	b.addImage("img-a", []byte(`{"config":"a"}`), [][]byte{[]byte("layer-a")})
	b.writeIndex()

	now := time.Now()
	// Orphan written recently — within grace period.
	orphan := b.writeOrphan([]byte("still-being-pulled"), now.Add(-30*time.Second))

	c := newCollectorForTest(t, dataDir, time.Minute, now)
	stats, err := c.Collect(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 1, stats.SkippedRecent)
	assert.Equal(t, 0, stats.DeletedBlobs)

	_, err = os.Stat(filepath.Join(b.blobsDir, orphan))
	assert.NoError(t, err, "recent orphan should be preserved")
}

func TestCollectIgnoresTempAndNonBlobFiles(t *testing.T) {
	dataDir := t.TempDir()
	b := newLayoutBuilder(t, dataDir)
	b.addImage("img-a", []byte(`{"c":1}`), [][]byte{[]byte("layer")})
	b.writeIndex()

	// Simulate an in-progress BlobStore.Put: <hex>.tmp file.
	tmpName := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.tmp"
	require.NoError(t, os.WriteFile(filepath.Join(b.blobsDir, tmpName), []byte("partial"), 0o644))

	// Also something unexpected with a wrong-length name.
	require.NoError(t, os.WriteFile(filepath.Join(b.blobsDir, "not-a-blob"), []byte("x"), 0o644))

	// Make both files "old" so the grace period doesn't protect them.
	past := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(b.blobsDir, tmpName), past, past))
	require.NoError(t, os.Chtimes(filepath.Join(b.blobsDir, "not-a-blob"), past, past))

	c := newCollectorForTest(t, dataDir, time.Minute, time.Now())
	stats, err := c.Collect(context.Background())
	require.NoError(t, err)

	// Neither the .tmp file nor the non-hex name should be touched.
	assert.Equal(t, 0, stats.DeletedBlobs)
	_, err = os.Stat(filepath.Join(b.blobsDir, tmpName))
	assert.NoError(t, err)
	_, err = os.Stat(filepath.Join(b.blobsDir, "not-a-blob"))
	assert.NoError(t, err)
}

func TestCollectFollowsManifestIndex(t *testing.T) {
	dataDir := t.TempDir()
	p := paths.New(dataDir)
	blobsDir := p.OCICacheBlobDir()
	require.NoError(t, os.MkdirAll(blobsDir, 0o755))

	// Build an inner image: config + layer + manifest.
	writeBlob := func(content []byte) string {
		sum := sha256.Sum256(content)
		hexDigest := hex.EncodeToString(sum[:])
		require.NoError(t, os.WriteFile(filepath.Join(blobsDir, hexDigest), content, 0o644))
		return "sha256:" + hexDigest
	}

	configContent := []byte(`{"inner-config":true}`)
	layerContent := []byte("inner-layer")
	configDigest := writeBlob(configContent)
	layerDigest := writeBlob(layerContent)

	innerManifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": configDigest, "size": len(configContent)},
		"layers":        []map[string]any{{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": layerDigest, "size": len(layerContent)}},
	}
	innerBytes, err := json.Marshal(innerManifest)
	require.NoError(t, err)
	innerDigest := writeBlob(innerBytes)

	// Build an outer manifest index that references the inner manifest.
	outerIndex := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     []map[string]any{{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": innerDigest, "size": len(innerBytes)}},
	}
	outerBytes, err := json.Marshal(outerIndex)
	require.NoError(t, err)
	outerDigest := writeBlob(outerBytes)

	// Cache index.json points at the outer manifest index.
	index := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     []map[string]any{{"mediaType": "application/vnd.oci.image.index.v1+json", "digest": outerDigest, "size": len(outerBytes)}},
	}
	indexBytes, err := json.Marshal(index)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p.OCICacheIndex(), indexBytes, 0o644))

	// Drop an unrelated orphan to verify it still gets swept.
	orphan := writeBlob([]byte("orphan-bytes"))
	// writeBlob returns a sha256: prefix; we need the hex for os.Stat.
	orphanHex := orphan[7:]
	// Force past the grace period.
	past := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(blobsDir, orphanHex), past, past))

	c := newCollectorForTest(t, dataDir, time.Minute, time.Now())
	stats, err := c.Collect(context.Background())
	require.NoError(t, err)

	// Live: outer index + inner manifest + config + layer = 4.
	assert.Equal(t, 4, stats.LiveBlobs)
	assert.Equal(t, 1, stats.DeletedBlobs, "only the orphan should be deleted")
	_, err = os.Stat(filepath.Join(blobsDir, orphanHex))
	assert.True(t, os.IsNotExist(err))
}

func TestNewCollectorValidatesArgs(t *testing.T) {
	dataDir := t.TempDir()
	p := paths.New(dataDir)

	_, err := NewCollector(nil, time.Hour, time.Minute, nil, nil)
	assert.Error(t, err)

	_, err = NewCollector(p, 0, time.Minute, nil, nil)
	assert.Error(t, err)

	_, err = NewCollector(p, time.Hour, -time.Minute, nil, nil)
	assert.Error(t, err)

	_, err = NewCollector(p, time.Hour, time.Minute, nil, nil)
	assert.NoError(t, err)
}
