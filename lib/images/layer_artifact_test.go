package images

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	gcr "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/opencontainers/umoci/oci/layer"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// whiteoutPrefix marks OCI whiteout entries (".wh.<name>" and ".wh..wh..opq").
// umoci interprets them during extraction: composeOnDiskFormat applies them
// against the tree being composed, layerArtifactOnDiskFormat converts them to
// overlayfs whiteout inodes and opaque xattrs so per-layer artifacts can
// later be stacked.
const whiteoutPrefix = ".wh."

// composeOnDiskFormat applies whiteouts against the tree being composed. It
// belongs to the composition flow and moves to production with that change.
func composeOnDiskFormat() layer.OnDiskFormat {
	return layer.DirRootfs{MapOptions: layerMapOptions()}
}

const testTarGzMediaType = "application/vnd.oci.image.layer.v1.tar+gzip"

// writeLayerTestLayout writes img into the shared OCI cache of p tagged with
// the image's digest, mirroring pullToOCILayout.
func writeLayerTestLayout(t *testing.T, p *paths.Paths, img gcr.Image) {
	t.Helper()
	digest, err := img.Digest()
	require.NoError(t, err)
	layoutPath, err := layout.Write(p.SystemOCICache(), empty.Index)
	require.NoError(t, err)
	require.NoError(t, layoutPath.AppendImage(img, layout.WithAnnotations(map[string]string{
		"org.opencontainers.image.ref.name": digestToLayoutTag(digest.String()),
	})))
}

func layerDescFromImage(t *testing.T, img gcr.Image, index int) layerDescriptor {
	t.Helper()
	manifest, err := img.Manifest()
	require.NoError(t, err)
	configFile, err := img.ConfigFile()
	require.NoError(t, err)
	layer := manifest.Layers[index]
	return layerDescriptor{
		Digest:    layer.Digest.String(),
		Size:      layer.Size,
		MediaType: string(layer.MediaType),
		DiffID:    configFile.RootFS.DiffIDs[index].String(),
	}
}

// tarGz builds a gzipped tar from the entries written by fn.
func tarGz(t *testing.T, fn func(tw *tar.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	fn(tw)
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())
	return buf.Bytes()
}

func writeTarEntry(t *testing.T, tw *tar.Writer, header *tar.Header, content string) {
	t.Helper()
	if header.Typeflag == tar.TypeReg {
		header.Size = int64(len(content))
	}
	require.NoError(t, tw.WriteHeader(header))
	if content != "" {
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
}

func fileEntry(name, content string) func(*testing.T, *tar.Writer) {
	return func(t *testing.T, tw *tar.Writer) {
		writeTarEntry(t, tw, &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0644}, content)
	}
}

func dirEntry(name string) func(*testing.T, *tar.Writer) {
	return func(t *testing.T, tw *tar.Writer) {
		writeTarEntry(t, tw, &tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0755}, "")
	}
}

func symlinkEntry(name, target string) func(*testing.T, *tar.Writer) {
	return func(t *testing.T, tw *tar.Writer) {
		writeTarEntry(t, tw, &tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target}, "")
	}
}

func hardlinkEntry(name, target string) func(*testing.T, *tar.Writer) {
	return func(t *testing.T, tw *tar.Writer) {
		writeTarEntry(t, tw, &tar.Header{Name: name, Typeflag: tar.TypeLink, Linkname: target}, "")
	}
}

// writeLayerBlob writes a gzipped tar layer to a file and returns its path.
func writeLayerBlob(t *testing.T, dir, name string, entries ...func(*testing.T, *tar.Writer)) string {
	t.Helper()
	data := tarGz(t, func(tw *tar.Writer) {
		for _, entry := range entries {
			entry(t, tw)
		}
	})
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0644))
	return path
}

// unpackInto applies a layer blob onto dest with compose semantics.
func unpackInto(t *testing.T, blobPath, dest string) *unpackStats {
	t.Helper()
	stats, err := unpackLayerBlob(context.Background(), blobPath, testTarGzMediaType, dest, composeOnDiskFormat())
	require.NoError(t, err)
	return stats
}

func requireNoWhiteoutMarkers(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		require.NoError(t, err)
		require.NotContains(t, entry.Name(), whiteoutPrefix, "whiteout marker leaked into tree")
		return nil
	}))
}

func TestMaterializeLayerArtifact(t *testing.T) {
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs not available")
	}

	p := paths.New(t.TempDir())
	img, err := mutate.AppendLayers(empty.Image, syntheticLayer(t, "base.txt", "base layer content"))
	require.NoError(t, err)
	writeLayerTestLayout(t, p, img)

	desc := layerDescFromImage(t, img, 0)
	m := &manager{paths: p}

	record, err := m.materializeLayerArtifact(context.Background(), desc)
	require.NoError(t, err)
	require.Equal(t, desc.Digest, record.Digest)
	require.Equal(t, desc.DiffID, record.DiffID)
	require.Equal(t, layerArtifactFormat(), record.Format)
	require.Greater(t, record.SizeBytes, int64(0))
	require.Greater(t, record.UnpackedBytes, int64(0))

	layerHex := desc.Digest[len("sha256:"):]
	artifactInfo, err := os.Stat(p.ImageLayerArtifactForFormat(layerHex, layerArtifactFormat()))
	require.NoError(t, err, "layer.erofs must be installed")

	// A second materialization reuses the existing artifact.
	reused, err := m.materializeLayerArtifact(context.Background(), desc)
	require.NoError(t, err)
	require.True(t, record.CreatedAt.Equal(reused.CreatedAt), "reuse must return the stored record")
	artifactInfoAfter, err := os.Stat(p.ImageLayerArtifactForFormat(layerHex, layerArtifactFormat()))
	require.NoError(t, err)
	require.Equal(t, artifactInfo.ModTime(), artifactInfoAfter.ModTime(), "reuse must not rebuild")
}

func TestMaterializeLayerArtifactRecoversCorruptRecord(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not available")
	}
	originalFormat := DefaultImageFormat
	DefaultImageFormat = FormatExt4
	t.Cleanup(func() { DefaultImageFormat = originalFormat })

	p := paths.New(t.TempDir())
	img, err := mutate.AppendLayers(empty.Image, syntheticLayer(t, "base.txt", "base layer content"))
	require.NoError(t, err)
	writeLayerTestLayout(t, p, img)

	desc := layerDescFromImage(t, img, 0)
	m := &manager{paths: p}
	first, err := m.materializeLayerArtifact(context.Background(), desc)
	require.NoError(t, err)

	layerHex := desc.Digest[len("sha256:"):]
	require.NoError(t, os.WriteFile(
		p.ImageLayerRecordForFormat(layerHex, layerArtifactFormat()),
		[]byte("{not valid json"),
		0600,
	))

	second, err := m.materializeLayerArtifact(context.Background(), desc)
	require.NoError(t, err)
	require.NotEqual(t, first.CreatedAt, second.CreatedAt, "corrupt record must trigger a rebuild")
	require.FileExists(t, p.ImageLayerArtifactForFormat(layerHex, layerArtifactFormat()))

	data, err := os.ReadFile(p.ImageLayerRecordForFormat(layerHex, layerArtifactFormat()))
	require.NoError(t, err)
	var record layerArtifact
	require.NoError(t, json.Unmarshal(data, &record))
	require.NoError(t, record.validate())
}

// TestMaterializeLayerArtifactOutlivesCancelledCaller checks that a caller
// abandoning a shared build does not abort the build for everyone else: the
// artifact still lands even though the initiating context was cancelled.
func TestMaterializeLayerArtifactOutlivesCancelledCaller(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not available")
	}
	originalFormat := DefaultImageFormat
	DefaultImageFormat = FormatExt4
	t.Cleanup(func() { DefaultImageFormat = originalFormat })

	p := paths.New(t.TempDir())
	img, err := mutate.AppendLayers(empty.Image, syntheticLayer(t, "base.txt", "base layer content"))
	require.NoError(t, err)
	writeLayerTestLayout(t, p, img)
	desc := layerDescFromImage(t, img, 0)
	m := &manager{paths: p}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The build usually loses the race against the cancelled context, but a
	// fast build can finish first; either outcome is valid when detached.
	if _, buildErr := m.materializeLayerArtifact(ctx, desc); buildErr != nil {
		require.ErrorIs(t, buildErr, context.Canceled)
	}

	layerHex := desc.Digest[len("sha256:"):]
	require.Eventually(t, func() bool {
		record, err := readLayerRecord(p, layerHex)
		return err == nil && record != nil
	}, 30*time.Second, 50*time.Millisecond, "detached build must still install the artifact")
	require.FileExists(t, p.ImageLayerArtifactForFormat(layerHex, layerArtifactFormat()))
}

func TestMaterializeLayerArtifactMissingBlob(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}

	_, err := m.materializeLayerArtifact(context.Background(), layerDescriptor{
		Digest:    "sha256:abababababababababababababababababababababababababababababababab",
		MediaType: testTarGzMediaType,
	})
	require.ErrorContains(t, err, "missing from oci cache")
}

// TestUnpackLayerBlobArtifactFormatKeepsWhiteouts checks that the artifact
// extraction format records deletions in overlayfs form: a 0:0 character
// device for a whiteout and the opaque xattr for an opaque directory.
func TestUnpackLayerBlobArtifactFormatKeepsWhiteouts(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("overlayfs whiteouts need mknod and trusted xattrs")
	}
	root := t.TempDir()
	blob := writeLayerBlob(t, root, "layer.tar.gz",
		fileEntry("keep.txt", "keep"),
		dirEntry("gone/"),
		fileEntry("gone/.wh.deleted.txt", ""),
		dirEntry("opq/"),
		fileEntry("opq/.wh..wh..opq", ""),
		fileEntry("opq/fresh.txt", "fresh"),
	)
	dest := filepath.Join(root, "dest")
	_, err := unpackLayerBlob(context.Background(), blob, testTarGzMediaType, dest, layerArtifactOnDiskFormat())
	require.NoError(t, err)

	var stat unix.Stat_t
	require.NoError(t, unix.Lstat(filepath.Join(dest, "gone", "deleted.txt"), &stat))
	require.Equal(t, uint32(unix.S_IFCHR), stat.Mode&unix.S_IFMT, "whiteout must be a character device")
	require.Equal(t, uint64(0), uint64(stat.Rdev), "whiteout device must be 0:0")

	value := make([]byte, 8)
	n, err := unix.Lgetxattr(filepath.Join(dest, "opq"), "trusted.overlay.opaque", value)
	require.NoError(t, err)
	require.Equal(t, "y", string(value[:n]))
	requireNoWhiteoutMarkers(t, dest)
}

func TestUnpackLayerBlobAppliesWhiteoutsAcrossLayers(t *testing.T) {
	root := t.TempDir()
	base := writeLayerBlob(t, root, "base.tar.gz",
		dirEntry("etc/"),
		fileEntry("etc/config.txt", "original"),
		dirEntry("data/"),
		fileEntry("data/old.txt", "stale"),
		dirEntry("replacedir/"),
		fileEntry("replacedir/inner.txt", "inner"),
		fileEntry("added/foo", "old"),
	)
	top := writeLayerBlob(t, root, "top.tar.gz",
		fileEntry("etc/.wh.config.txt", ""),
		fileEntry("data/.wh..wh..opq", ""),
		fileEntry("data/new.txt", "new"),
		fileEntry("replacedir", "now a file"),
		fileEntry("added/.wh.foo", ""),
		fileEntry("added/foo", "new"),
	)
	dest := filepath.Join(root, "dest")
	unpackInto(t, base, dest)
	unpackInto(t, top, dest)

	_, err := os.Lstat(filepath.Join(dest, "etc", "config.txt"))
	require.True(t, os.IsNotExist(err), "whiteout must delete the lower entry")
	_, err = os.Lstat(filepath.Join(dest, "data", "old.txt"))
	require.True(t, os.IsNotExist(err), "opaque marker must mask lower contents")
	data, err := os.ReadFile(filepath.Join(dest, "data", "new.txt"))
	require.NoError(t, err)
	require.Equal(t, "new", string(data))
	info, err := os.Lstat(filepath.Join(dest, "replacedir"))
	require.NoError(t, err)
	require.False(t, info.IsDir(), "file must replace lower directory")
	data, err = os.ReadFile(filepath.Join(dest, "added", "foo"))
	require.NoError(t, err)
	require.Equal(t, "new", string(data), "whiteout then recreate in one layer keeps the new file")
	requireNoWhiteoutMarkers(t, dest)
}

// TestUnpackLayerBlobReplacesDirectorySymlink checks the standard runtime
// behavior: a directory in an upper layer replaces a symlink from a lower one
// rather than writing through it.
func TestUnpackLayerBlobReplacesDirectorySymlink(t *testing.T) {
	root := t.TempDir()
	base := writeLayerBlob(t, root, "base.tar.gz",
		dirEntry("usr/"),
		dirEntry("usr/bin/"),
		fileEntry("usr/bin/sh", "sh"),
		symlinkEntry("bin", "usr/bin"),
	)
	top := writeLayerBlob(t, root, "top.tar.gz",
		dirEntry("bin/"),
		fileEntry("bin/tool", "tool"),
	)
	dest := filepath.Join(root, "dest")
	unpackInto(t, base, dest)
	unpackInto(t, top, dest)

	info, err := os.Lstat(filepath.Join(dest, "bin"))
	require.NoError(t, err)
	require.True(t, info.IsDir(), "upper directory must replace the lower symlink")
	require.FileExists(t, filepath.Join(dest, "bin", "tool"))
	require.FileExists(t, filepath.Join(dest, "usr", "bin", "sh"))
	require.NoFileExists(t, filepath.Join(dest, "usr", "bin", "tool"))
}

func TestUnpackLayerBlobConfinesSymlinkTraversal(t *testing.T) {
	root := t.TempDir()
	blob := writeLayerBlob(t, root, "layer.tar.gz",
		symlinkEntry("link", "../../escape"),
		fileEntry("link/pwned", "x"),
	)
	dest := filepath.Join(root, "dest")
	unpackInto(t, blob, dest)

	require.NoFileExists(t, filepath.Join(root, "escape", "pwned"))
	require.FileExists(t, filepath.Join(dest, "escape", "pwned"), "escaping link must be resolved inside the root")
}

func TestUnpackLayerBlobPreservesHardlinks(t *testing.T) {
	root := t.TempDir()
	blob := writeLayerBlob(t, root, "layer.tar.gz",
		fileEntry("a", "shared"),
		hardlinkEntry("b", "a"),
	)
	dest := filepath.Join(root, "dest")
	unpackInto(t, blob, dest)

	a, err := os.Stat(filepath.Join(dest, "a"))
	require.NoError(t, err)
	b, err := os.Stat(filepath.Join(dest, "b"))
	require.NoError(t, err)
	require.True(t, os.SameFile(a, b))
}

func TestUnpackLayerBlobContextHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	blob := writeLayerBlob(t, root, "layer.tar.gz", fileEntry("file", "x"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := unpackLayerBlob(ctx, blob, testTarGzMediaType, filepath.Join(root, "dest"), composeOnDiskFormat())
	require.ErrorIs(t, err, context.Canceled)
}

func TestUnpackLayerBlobIncludesTrailingTarPaddingInDiffID(t *testing.T) {
	root := t.TempDir()
	var tarData bytes.Buffer
	tw := tar.NewWriter(&tarData)
	writeTarEntry(t, tw, &tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0644}, "x")
	require.NoError(t, tw.Close())
	_, err := tarData.Write(make([]byte, 512))
	require.NoError(t, err)

	var compressed bytes.Buffer
	gzw := gzip.NewWriter(&compressed)
	_, err = gzw.Write(tarData.Bytes())
	require.NoError(t, err)
	require.NoError(t, gzw.Close())
	blob := filepath.Join(root, "layer.tar.gz")
	require.NoError(t, os.WriteFile(blob, compressed.Bytes(), 0644))

	stats := unpackInto(t, blob, filepath.Join(root, "dest"))
	want := sha256.Sum256(tarData.Bytes())
	require.Equal(t, "sha256:"+fmt.Sprintf("%x", want), stats.diffID)
	require.Equal(t, int64(tarData.Len()), stats.unpackedBytes)
}
