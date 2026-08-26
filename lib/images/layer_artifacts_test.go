package images

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kernel/hypeman/lib/ocicache"
	"github.com/kernel/hypeman/lib/ocicache/testutil"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

// testLayer is a v1.Layer over explicit byte slices so tests control the
// exact compressed and uncompressed content and media type.
type testLayer struct {
	uncompressed []byte
	compressed   []byte
	mediaType    types.MediaType
}

func hashOf(b []byte) v1.Hash {
	sum := sha256.Sum256(b)
	return v1.Hash{Algorithm: "sha256", Hex: fmt.Sprintf("%x", sum)}
}

func (l *testLayer) Digest() (v1.Hash, error)            { return hashOf(l.compressed), nil }
func (l *testLayer) DiffID() (v1.Hash, error)            { return hashOf(l.uncompressed), nil }
func (l *testLayer) Size() (int64, error)                { return int64(len(l.compressed)), nil }
func (l *testLayer) MediaType() (types.MediaType, error) { return l.mediaType, nil }
func (l *testLayer) Compressed() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(l.compressed)), nil
}
func (l *testLayer) Uncompressed() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(l.uncompressed)), nil
}

type tarEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
}

func buildTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Linkname: e.linkname,
			ModTime:  time.Unix(0, 0),
		}
		switch e.typeflag {
		case tar.TypeDir:
			hdr.Mode = 0755
		case tar.TypeReg:
			hdr.Mode = 0644
			hdr.Size = int64(len(e.body))
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if e.typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(b)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func newGzipLayer(t *testing.T, entries []tarEntry) *testLayer {
	t.Helper()
	uncompressed := buildTar(t, entries)
	return &testLayer{
		uncompressed: uncompressed,
		compressed:   gzipBytes(t, uncompressed),
		mediaType:    types.OCILayer,
	}
}

func buildTestImage(t *testing.T, layers ...v1.Layer) v1.Image {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, layers...)
	require.NoError(t, err)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	return img
}

// writeTestImage stores an image in the OCI cache under p and returns its
// manifest digest and the config diff IDs in layer order.
func writeTestImage(t *testing.T, p *paths.Paths, layers ...v1.Layer) (string, []v1.Hash) {
	t.Helper()
	img := buildTestImage(t, layers...)
	digest, err := testutil.WriteImage(p, img)
	require.NoError(t, err)
	config, err := img.ConfigFile()
	require.NoError(t, err)
	return digest, config.RootFS.DiffIDs
}

func requireErofsTooling(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("erofs layer artifacts require a Linux host")
	}
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs not installed")
	}
}

// extractErofs extracts an erofs artifact into a fresh directory using
// fsck.erofs. Skips the test when the tool is unavailable.
func extractErofs(t *testing.T, artifactPath string) string {
	t.Helper()
	if _, err := exec.LookPath("fsck.erofs"); err != nil {
		t.Skip("fsck.erofs not installed")
	}
	outDir := t.TempDir()
	cmd := exec.Command("fsck.erofs", "--extract="+outDir, artifactPath)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "fsck.erofs extract: %s", output)
	return outDir
}

func TestExportLayerArtifactsUnsupportedTooling(t *testing.T) {
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("mkfs.erofs"); err == nil {
			t.Skip("erofs tooling is available on this host")
		}
	}
	p := paths.New(t.TempDir())
	_, err := ExportLayerArtifacts(context.Background(), p, "sha256:"+strings.Repeat("a", 64))
	require.ErrorIs(t, err, ErrLayerArtifactsUnsupported)
}

func TestExportLayerArtifactsImageNotFound(t *testing.T) {
	requireErofsTooling(t)
	p := paths.New(t.TempDir())
	_, err := ExportLayerArtifacts(context.Background(), p, "sha256:"+strings.Repeat("a", 64))
	require.ErrorIs(t, err, ocicache.ErrNotFound)
}

func TestExportLayerArtifactsHappyPath(t *testing.T) {
	requireErofsTooling(t)
	p := paths.New(t.TempDir())

	layer1 := newGzipLayer(t, []tarEntry{
		{name: "base/", typeflag: tar.TypeDir},
		{name: "base/hello.txt", body: "hello", typeflag: tar.TypeReg},
	})
	layer2 := newGzipLayer(t, []tarEntry{
		{name: "upper/", typeflag: tar.TypeDir},
		{name: "upper/world.txt", body: "world", typeflag: tar.TypeReg},
	})
	digest, diffIDs := writeTestImage(t, p, layer1, layer2)

	report, err := ExportLayerArtifacts(context.Background(), p, digest)
	require.NoError(t, err)

	require.Equal(t, "sha256:"+strings.TrimPrefix(digest, "sha256:"), report.ImageDigest)
	require.Empty(t, report.Skipped)
	require.Len(t, report.Artifacts, 2)

	for i, artifact := range report.Artifacts {
		diffID := diffIDs[i]
		require.Equal(t, i, artifact.Index)
		require.Equal(t, diffID.String(), artifact.DiffID)
		require.Equal(t, p.LayerArtifactPath(diffID.Hex), artifact.ArtifactPath)
		require.False(t, artifact.Reused)

		info, err := os.Stat(artifact.ArtifactPath)
		require.NoError(t, err)
		require.Equal(t, info.Size(), artifact.SizeBytes)
		require.Greater(t, artifact.SizeBytes, int64(0))

		data, err := os.ReadFile(p.LayerArtifactMetadata(diffID.Hex))
		require.NoError(t, err)
		var meta layerArtifactMetadata
		require.NoError(t, json.Unmarshal(data, &meta))
		require.Equal(t, artifact.LayerDigest, meta.LayerDigest)
		require.Equal(t, diffID.String(), meta.DiffID)
		require.Equal(t, FormatErofs, meta.Format)
		require.Equal(t, artifact.SizeBytes, meta.SizeBytes)
	}

	// Artifact content matches the layer's contribution.
	layer1Root := extractErofs(t, report.Artifacts[0].ArtifactPath)
	content, err := os.ReadFile(filepath.Join(layer1Root, "base", "hello.txt"))
	require.NoError(t, err)
	require.Equal(t, "hello", string(content))
	layer2Root := extractErofs(t, report.Artifacts[1].ArtifactPath)
	content, err = os.ReadFile(filepath.Join(layer2Root, "upper", "world.txt"))
	require.NoError(t, err)
	require.Equal(t, "world", string(content))

	// The exporter must not touch the flattened image layouts.
	_, err = os.Stat(filepath.Join(p.ImagesDir(), "content"))
	require.True(t, os.IsNotExist(err))
}

func TestExportLayerArtifactsReusesExistingArtifacts(t *testing.T) {
	requireErofsTooling(t)
	p := paths.New(t.TempDir())

	layer := newGzipLayer(t, []tarEntry{
		{name: "a.txt", body: "a", typeflag: tar.TypeReg},
	})
	digest, _ := writeTestImage(t, p, layer)

	first, err := ExportLayerArtifacts(context.Background(), p, digest)
	require.NoError(t, err)
	require.Len(t, first.Artifacts, 1)
	require.False(t, first.Artifacts[0].Reused)

	// Reuse heals metadata lost between the artifact and metadata installs.
	require.NoError(t, os.Remove(p.LayerArtifactMetadata(strings.TrimPrefix(first.Artifacts[0].DiffID, "sha256:"))))

	second, err := ExportLayerArtifacts(context.Background(), p, digest)
	require.NoError(t, err)
	require.Len(t, second.Artifacts, 1)
	require.True(t, second.Artifacts[0].Reused)
	require.Equal(t, first.Artifacts[0].SizeBytes, second.Artifacts[0].SizeBytes)
	_, err = os.Stat(p.LayerArtifactMetadata(strings.TrimPrefix(first.Artifacts[0].DiffID, "sha256:")))
	require.NoError(t, err)

	// No unpack scratch directories are left behind.
	entries, err := os.ReadDir(p.LayerArtifactsDir())
	require.NoError(t, err)
	for _, entry := range entries {
		require.False(t, strings.HasPrefix(entry.Name(), ".unpack-"), "leftover scratch dir %s", entry.Name())
	}
}

func TestExportLayerArtifactsSkipsUnsupportedMediaType(t *testing.T) {
	requireErofsTooling(t)
	p := paths.New(t.TempDir())

	supported := newGzipLayer(t, []tarEntry{
		{name: "a.txt", body: "a", typeflag: tar.TypeReg},
	})
	// The exporter decides by media type before reading the blob, so the
	// body never needs to be valid zstd.
	zstdUncompressed := buildTar(t, []tarEntry{
		{name: "b.txt", body: "b", typeflag: tar.TypeReg},
	})
	zstd := &testLayer{
		uncompressed: zstdUncompressed,
		compressed:   zstdUncompressed,
		mediaType:    types.OCILayerZStd,
	}
	digest, _ := writeTestImage(t, p, supported, zstd)

	report, err := ExportLayerArtifacts(context.Background(), p, digest)
	require.NoError(t, err)
	require.Len(t, report.Artifacts, 1)
	require.Equal(t, 0, report.Artifacts[0].Index)
	_, err = os.Stat(report.Artifacts[0].ArtifactPath)
	require.NoError(t, err)

	require.Len(t, report.Skipped, 1)
	require.Equal(t, 1, report.Skipped[0].Index)
	require.Contains(t, report.Skipped[0].Reason, string(types.OCILayerZStd))
}

func TestExportLayerArtifactsSkipsCrossLayerHardlink(t *testing.T) {
	requireErofsTooling(t)
	p := paths.New(t.TempDir())

	layer1 := newGzipLayer(t, []tarEntry{
		{name: "base.txt", body: "base", typeflag: tar.TypeReg},
	})
	// Hardlinks resolve against the unpack root only; the target lives in an
	// earlier layer, so this layer cannot be unpacked standalone.
	layer2 := newGzipLayer(t, []tarEntry{
		{name: "link.txt", typeflag: tar.TypeLink, linkname: "base.txt"},
	})
	digest, diffIDs := writeTestImage(t, p, layer1, layer2)

	report, err := ExportLayerArtifacts(context.Background(), p, digest)
	require.NoError(t, err)

	require.Len(t, report.Artifacts, 1)
	require.Equal(t, 0, report.Artifacts[0].Index)
	require.Len(t, report.Skipped, 1)
	require.Equal(t, 1, report.Skipped[0].Index)
	require.Contains(t, report.Skipped[0].Reason, "unpack layer standalone")

	// A failed layer must not leave a partial artifact behind.
	_, err = os.Stat(p.LayerArtifactPath(diffIDs[1].Hex))
	require.True(t, os.IsNotExist(err))
}

func TestExportLayerArtifactsDropsLowerLayerWhiteouts(t *testing.T) {
	requireErofsTooling(t)
	p := paths.New(t.TempDir())

	layer1 := newGzipLayer(t, []tarEntry{
		{name: "base.txt", body: "base", typeflag: tar.TypeReg},
	})
	// Whiteout of a path introduced by the lower layer: umoci applies it as
	// a removal against the (empty) unpack root, so it is a no-op and the
	// layer's own contribution still converts. See the whiteout limitation
	// documented on ExportLayerArtifacts.
	layer2 := newGzipLayer(t, []tarEntry{
		{name: ".wh.base.txt", typeflag: tar.TypeReg},
		{name: "new.txt", body: "new", typeflag: tar.TypeReg},
	})
	digest, _ := writeTestImage(t, p, layer1, layer2)

	report, err := ExportLayerArtifacts(context.Background(), p, digest)
	require.NoError(t, err)
	require.Empty(t, report.Skipped)
	require.Len(t, report.Artifacts, 2)

	layer2Root := extractErofs(t, report.Artifacts[1].ArtifactPath)
	content, err := os.ReadFile(filepath.Join(layer2Root, "new.txt"))
	require.NoError(t, err)
	require.Equal(t, "new", string(content))
	_, err = os.Stat(filepath.Join(layer2Root, "base.txt"))
	require.True(t, os.IsNotExist(err))
}

func TestExportLayerArtifactsDiffIDMismatch(t *testing.T) {
	requireErofsTooling(t)
	p := paths.New(t.TempDir())

	layer := newGzipLayer(t, []tarEntry{
		{name: "a.txt", body: "a", typeflag: tar.TypeReg},
	})
	digest, diffIDs := writeTestImage(t, p, layer)

	// Corrupt the cached blob with different-but-valid layer content: the
	// unpack succeeds but the stream no longer hashes to the config's diff ID.
	other := newGzipLayer(t, []tarEntry{
		{name: "b.txt", body: "b", typeflag: tar.TypeReg},
	})
	layerDigest, err := layer.Digest()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p.OCICacheBlob(layerDigest.Hex), other.compressed, 0644))

	_, err = ExportLayerArtifacts(context.Background(), p, digest)
	require.Error(t, err)
	require.Contains(t, err.Error(), "diff ID mismatch")

	// A failed verification must not install an artifact.
	_, err = os.Stat(p.LayerArtifactPath(diffIDs[0].Hex))
	require.True(t, os.IsNotExist(err))
}

func TestExportLayerArtifactsMissingLayerBlob(t *testing.T) {
	requireErofsTooling(t)
	p := paths.New(t.TempDir())

	layer := newGzipLayer(t, []tarEntry{
		{name: "a.txt", body: "a", typeflag: tar.TypeReg},
	})
	digest, _ := writeTestImage(t, p, layer)

	layerDigest, err := layer.Digest()
	require.NoError(t, err)
	require.NoError(t, os.Remove(p.OCICacheBlob(layerDigest.Hex)))

	_, err = ExportLayerArtifacts(context.Background(), p, digest)
	require.Error(t, err)
	require.Contains(t, err.Error(), "layer blob missing")
}
