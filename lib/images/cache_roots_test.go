package images

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/ocicachegc"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedMetadataWithLayers(t *testing.T, p *paths.Paths, imageRef, status string, layers []LayerRef) *imageMetadata {
	t.Helper()

	ref, err := ParseNormalizedRef(imageRef)
	require.NoError(t, err)

	meta := &imageMetadata{
		Name:      imageRef,
		Digest:    ref.Digest(),
		Status:    status,
		BuildID:   "build-" + ref.DigestHex(),
		Layers:    layers,
		CreatedAt: time.Now(),
	}
	require.NoError(t, writeMetadata(p, ref.Repository(), ref.DigestHex(), meta))
	if status == StatusReady {
		// Ready metadata is only readable when its disk file exists.
		layout := resolveImageLayout(p, ref.Repository(), ref.DigestHex())
		require.NoError(t, os.MkdirAll(filepath.Dir(layout.disk), 0o755))
		require.NoError(t, os.WriteFile(layout.disk, []byte("rootfs"), 0o644))
	}
	return meta
}

func TestLiveOCICacheDigestsProtectsReferencedAndInflightArtifacts(t *testing.T) {
	p := paths.New(t.TempDir())

	sharedLayer := LayerRef{Digest: "sha256:" + sha256Hash([]byte("shared-layer")), Size: 10}
	readyOnlyLayer := LayerRef{Digest: "sha256:" + sha256Hash([]byte("ready-layer")), Size: 20}
	failedLayer := LayerRef{Digest: "sha256:" + sha256Hash([]byte("failed-layer")), Size: 30}

	ready := seedMetadataWithLayers(t, p,
		"docker.io/library/alpine@sha256:"+sha256Hash([]byte("ready")),
		StatusReady, []LayerRef{sharedLayer, readyOnlyLayer})
	inflight := seedMetadataWithLayers(t, p,
		"docker.io/library/nginx@sha256:"+sha256Hash([]byte("inflight")),
		StatusPulling, []LayerRef{sharedLayer})
	failed := seedMetadataWithLayers(t, p,
		"docker.io/library/busybox@sha256:"+sha256Hash([]byte("failed")),
		StatusFailed, []LayerRef{failedLayer})

	got := LiveOCICacheDigests(p)

	assert.Contains(t, got, ready.Digest)
	assert.Contains(t, got, sharedLayer.Digest)
	assert.Contains(t, got, readyOnlyLayer.Digest)
	// In-flight pulls carry only the manifest digest; their layers are not
	// recorded until finalize, but the manifest root protects blobs written
	// so far.
	assert.Contains(t, got, inflight.Digest)
	// Failed images contribute nothing so their blobs stay collectable.
	assert.NotContains(t, got, failed.Digest)
	assert.NotContains(t, got, failedLayer.Digest)

	// Shared digests appear once even though two images reference them.
	count := 0
	for _, digest := range got {
		if digest == sharedLayer.Digest {
			count++
		}
	}
	assert.Equal(t, 1, count)
}

func TestComputeLayerAccountingCountsSharedLayersOnce(t *testing.T) {
	sharedLayer := LayerRef{Digest: "sha256:" + sha256Hash([]byte("shared")), Size: 100}
	uniqueLayerA := LayerRef{Digest: "sha256:" + sha256Hash([]byte("a")), Size: 50}
	uniqueLayerB := LayerRef{Digest: "sha256:" + sha256Hash([]byte("b")), Size: 25}

	metas := []*imageMetadata{
		{Status: StatusReady, Layers: []LayerRef{sharedLayer, uniqueLayerA, sharedLayer}},
		{Status: StatusReady, Layers: []LayerRef{sharedLayer, uniqueLayerB}},
		{Status: StatusFailed, Layers: []LayerRef{{Digest: "sha256:" + sha256Hash([]byte("failed")), Size: 999}}},
	}

	accounting := computeLayerAccounting(metas)

	// 100 (shared) + 50 (a) + 25 (b); the duplicate sharedLayer within the
	// first image is counted once.
	assert.Equal(t, int64(175), accounting.uniqueBytes)
	assert.Equal(t, int64(100), accounting.sharedBytes)
}

// TestOCICacheGCProtectsImageReferencedBlobs runs a real GC sweep against a
// cache where one blob is referenced only by image metadata (not by
// index.json) and another is unreferenced. The metadata-referenced blob must
// survive; the orphan must be collected.
func TestOCICacheGCProtectsImageReferencedBlobs(t *testing.T) {
	p := paths.New(t.TempDir())

	blobDir := p.OCICacheBlobDir()
	require.NoError(t, os.MkdirAll(blobDir, 0o755))

	referencedBlob := sha256Hash([]byte("referenced-layer"))
	orphanBlob := sha256Hash([]byte("orphan-layer"))
	for _, blob := range []string{referencedBlob, orphanBlob} {
		require.NoError(t, os.WriteFile(filepath.Join(blobDir, blob), []byte("blob-"+blob), 0o644))
	}

	seedMetadataWithLayers(t, p,
		"docker.io/library/alpine@sha256:"+sha256Hash([]byte("ready")),
		StatusReady, []LayerRef{{Digest: "sha256:" + referencedBlob, Size: 4}})

	collector, err := ocicachegc.NewCollector(p, time.Hour, 0, NewOCICacheRoots(p), nil, nil, nil)
	require.NoError(t, err)

	stats, err := collector.Collect(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 1, stats.DeletedBlobs)
	assert.Equal(t, int64(len("blob-"+orphanBlob)), stats.DeletedBytes)
	assert.FileExists(t, filepath.Join(blobDir, referencedBlob))
	assert.NoFileExists(t, filepath.Join(blobDir, orphanBlob))
}

func TestFinalizeImageRecordsLayers(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}

	digestStr := "sha256:" + sha256Hash([]byte("finalize"))
	imageRef := "docker.io/library/alpine@" + digestStr
	ref, err := ParseNormalizedRef(imageRef)
	require.NoError(t, err)
	resolved := NewResolvedRef(ref, digestStr)

	require.NoError(t, writeMetadata(p, ref.Repository(), ref.DigestHex(), &imageMetadata{
		Name:      imageRef,
		Digest:    digestStr,
		Status:    StatusPending,
		BuildID:   "b1",
		CreatedAt: time.Now(),
	}))

	diskTempPath := filepath.Join(t.TempDir(), "rootfs.tmp")
	require.NoError(t, os.WriteFile(diskTempPath, []byte("disk"), 0o644))

	layers := []LayerRef{{Digest: "sha256:" + sha256Hash([]byte("layer-1")), Size: 5}}
	err = m.finalizeImage(resolved, &pullResult{Metadata: &containerMetadata{}, Layers: layers}, int64(len("disk")), "b1", diskTempPath)
	require.NoError(t, err)

	meta, err := readMetadata(p, ref.Repository(), ref.DigestHex())
	require.NoError(t, err)
	require.Equal(t, StatusReady, meta.Status)
	require.Equal(t, layers, meta.Layers)
}
