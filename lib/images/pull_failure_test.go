package images

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/queue"
	"github.com/stretchr/testify/require"
)

func TestExistsInLayoutRequiresEveryBlob(t *testing.T) {
	p := paths.New(t.TempDir())
	img, err := random.Image(256, 2)
	require.NoError(t, err)
	digest, err := img.Digest()
	require.NoError(t, err)
	tag := strings.TrimPrefix(digest.String(), "sha256:")

	lp, err := layout.Write(p.SystemOCICache(), empty.Index)
	require.NoError(t, err)
	require.NoError(t, lp.AppendImage(img, layout.WithAnnotations(map[string]string{"org.opencontainers.image.ref.name": tag})))
	client, err := newOCIClient(p.SystemOCICache())
	require.NoError(t, err)
	require.True(t, client.existsInLayout(tag))

	layers, err := img.Layers()
	require.NoError(t, err)
	layerDigest, err := layers[0].Digest()
	require.NoError(t, err)
	// The index entry survives; the layer blob does not.
	require.NoError(t, os.Remove(filepath.Join(p.SystemOCICache(), "blobs", "sha256", layerDigest.Hex)))
	require.False(t, client.existsInLayout(tag), "a stale index entry must not count as cached")
}

func TestFailedStatusSurvivesUnwritableMetadata(t *testing.T) {
	p, m, repo := newTagTestCase(t)
	m.queue = queue.New(1)
	m.readySubscribers = make(map[string][]chan StatusEvent)
	digest := strings.Repeat("ab", 32)
	ref, err := ParseNormalizedRef(repo + "@sha256:" + digest)
	require.NoError(t, err)
	resolved := NewResolvedRef(ref, "sha256:"+digest)
	dir := p.ImageContentDir(digest)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, writeMetadataFile(p.ImageContentMetadata(digest), &imageMetadata{
		Name: repo + "@sha256:" + digest, Digest: "sha256:" + digest, Status: StatusPulling,
		BuildID: "build-1", CreatedAt: time.Now().UTC(),
	}))

	// A full disk is modelled by a metadata directory that rejects new files.
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	m.updateStatusByDigest(resolved, StatusFailed, errors.New("unpack: no space left on device"), "build-1")

	got, err := m.GetImage(context.Background(), repo+"@sha256:"+digest)
	require.NoError(t, err)
	require.Equal(t, StatusFailed, got.Status, "the stored record is still pulling, but the failure must be reported")
	require.NotNil(t, got.Error)
	require.Equal(t, "unpack: no space left on device", *got.Error)

	// Once a write lands, the stored record is authoritative and the override clears.
	require.NoError(t, os.Chmod(dir, 0o755))
	m.updateStatusByDigest(resolved, StatusFailed, errors.New("second failure"), "build-1")
	got, err = m.GetImage(context.Background(), repo+"@sha256:"+digest)
	require.NoError(t, err)
	require.Equal(t, StatusFailed, got.Status)
	require.Equal(t, "second failure", *got.Error)
	_, tracked := m.persistFailures.Load(digest)
	require.False(t, tracked)
}

func TestDeleteReconcilesStalePendingRecord(t *testing.T) {
	p, m, repo := newTagTestCase(t)
	m.queue = queue.New(1)
	m.readySubscribers = make(map[string][]chan StatusEvent)
	digest := strings.Repeat("cd", 32)
	require.NoError(t, os.MkdirAll(p.ImageContentDir(digest), 0o755))
	require.NoError(t, os.WriteFile(p.ImageContentPath(digest), []byte("rootfs!"), 0o644))
	require.NoError(t, writeMetadataFile(p.ImageContentMetadata(digest), &imageMetadata{
		Name: repo + "@sha256:" + digest, Digest: "sha256:" + digest, Status: StatusPulling,
		BuildID: "build-1", CreatedAt: time.Now().UTC(),
	}))

	require.NoError(t, m.DeleteImage(context.Background(), repo+"@sha256:"+digest))
	_, err := os.Stat(p.ImageContentDir(digest))
	require.True(t, os.IsNotExist(err), "a pending record with no queued pull must not block deletion")
}

func TestDeleteKeepsContentOfQueuedPull(t *testing.T) {
	p, m, repo := newTagTestCase(t)
	m.queue = queue.New(1)
	m.readySubscribers = make(map[string][]chan StatusEvent)
	digest := strings.Repeat("ef", 32)
	require.NoError(t, os.MkdirAll(p.ImageContentDir(digest), 0o755))
	require.NoError(t, os.WriteFile(p.ImageContentPath(digest), []byte("rootfs!"), 0o644))
	require.NoError(t, writeMetadataFile(p.ImageContentMetadata(digest), &imageMetadata{
		Name: repo + "@sha256:" + digest, Digest: "sha256:" + digest, Status: StatusPulling,
		BuildID: "build-1", CreatedAt: time.Now().UTC(),
	}))
	block := make(chan struct{})
	defer close(block)
	m.queue.Enqueue("sha256:"+digest, func() { <-block }, nil)
	require.True(t, m.queue.Tracked("sha256:"+digest))

	require.NoError(t, m.DeleteImage(context.Background(), repo+"@sha256:"+digest))
	_, err := os.Stat(p.ImageContentDir(digest))
	require.NoError(t, err, "content of a pull that is still queued must not be removed")
}
