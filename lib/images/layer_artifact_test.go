package images

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	gcr "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

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

	record, err := m.materializeLayerArtifact(desc)
	require.NoError(t, err)
	require.Equal(t, desc.Digest, record.Digest)
	require.Equal(t, desc.DiffID, record.DiffID)
	require.Equal(t, layerArtifactFormat(), record.Format)
	require.Greater(t, record.SizeBytes, int64(0))
	require.Greater(t, record.UnpackedBytes, int64(0))
	require.Greater(t, record.Entries, 0)

	layerHex := desc.Digest[len("sha256:"):]
	_, err = os.Stat(p.ImageLayerArtifactForFormat(layerHex, layerArtifactFormat()))
	require.NoError(t, err, "layer.erofs must be installed")

	// A second materialization reuses the existing artifact.
	artifactInfo, err := os.Stat(p.ImageLayerArtifactForFormat(layerHex, layerArtifactFormat()))
	require.NoError(t, err)
	reused, err := m.materializeLayerArtifact(desc)
	require.NoError(t, err)
	require.True(t, record.CreatedAt.Equal(reused.CreatedAt), "reuse must return the stored record")
	artifactInfoAfter, err := os.Stat(p.ImageLayerArtifactForFormat(layerHex, layerArtifactFormat()))
	require.NoError(t, err)
	require.Equal(t, artifactInfo.ModTime(), artifactInfoAfter.ModTime(), "reuse must not rebuild")
}

func TestMaterializeLayerArtifactMissingBlob(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}

	_, err := m.materializeLayerArtifact(layerDescriptor{
		Digest:    "sha256:abababababababababababababababababababababababababababababababab",
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
	})
	require.ErrorContains(t, err, "missing from oci cache")
}

// whiteoutLayer builds a gzipped tar layer exercising whiteouts: a plain file,
// a whiteout marker, an opaque directory marker, and a whiteout+recreate pair.
func whiteoutLayer(t *testing.T) gcr.Layer {
	t.Helper()

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	writeEntry := func(header *tar.Header, content string) {
		require.NoError(t, tw.WriteHeader(header))
		if content != "" {
			_, err := tw.Write([]byte(content))
			require.NoError(t, err)
		}
	}
	writeEntry(&tar.Header{Name: "keep.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 4}, "keep")
	writeEntry(&tar.Header{Name: "gone/", Typeflag: tar.TypeDir, Mode: 0755}, "")
	writeEntry(&tar.Header{Name: "gone/.wh.deleted.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 0}, "")
	writeEntry(&tar.Header{Name: "opq/", Typeflag: tar.TypeDir, Mode: 0755}, "")
	writeEntry(&tar.Header{Name: "opq/.wh..wh..opq", Typeflag: tar.TypeReg, Mode: 0644, Size: 0}, "")
	writeEntry(&tar.Header{Name: "opq/fresh.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 5}, "fresh")
	writeEntry(&tar.Header{Name: "added/", Typeflag: tar.TypeDir, Mode: 0755}, "")
	writeEntry(&tar.Header{Name: "added/.wh.foo", Typeflag: tar.TypeReg, Mode: 0644, Size: 0}, "")
	writeEntry(&tar.Header{Name: "added/foo", Typeflag: tar.TypeReg, Mode: 0644, Size: 3}, "new")

	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())

	data := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	require.NoError(t, err)
	return layer
}

func TestMaterializeLayerRecordsWhiteouts(t *testing.T) {
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs not available")
	}

	p := paths.New(t.TempDir())
	img, err := mutate.AppendLayers(empty.Image, whiteoutLayer(t))
	require.NoError(t, err)
	writeLayerTestLayout(t, p, img)

	desc := layerDescFromImage(t, img, 0)
	m := &manager{paths: p}

	record, err := m.materializeLayerArtifact(desc)
	require.NoError(t, err)

	require.Contains(t, record.Whiteouts, whiteoutRecord{Dir: "gone", Target: "deleted.txt"})
	require.Contains(t, record.Whiteouts, whiteoutRecord{Dir: "opq", Opaque: true})
	require.Contains(t, record.Whiteouts, whiteoutRecord{Dir: "added", Target: "foo"})

	// Opaque and whiteout markers are recorded distinctly.
	opaqueCount := 0
	for _, whiteout := range record.Whiteouts {
		if whiteout.Opaque {
			opaqueCount++
			require.Empty(t, whiteout.Target)
		}
	}
	require.Equal(t, 1, opaqueCount)
}

func TestApplyLayerTreeWhiteoutSemantics(t *testing.T) {
	root := t.TempDir()
	targetDir := filepath.Join(root, "target")
	layerDir := filepath.Join(root, "layer")

	// Lower state contributed by earlier layers.
	require.NoError(t, os.MkdirAll(filepath.Join(targetDir, "opqdir"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(targetDir, "swapdir"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(targetDir, "victim.txt"), []byte("old"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(targetDir, "removedir"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(targetDir, "removedir", "inner.txt"), []byte("old"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(targetDir, "keep.txt"), []byte("old"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(targetDir, "opqdir", "stale.txt"), []byte("stale"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(targetDir, "swapdir", "inner.txt"), []byte("inner"), 0644))

	// Layer: whiteout victim.txt, opaque opqdir, replace swapdir with a file,
	// and whiteout-then-recreate added/foo within the same layer.
	require.NoError(t, os.MkdirAll(filepath.Join(layerDir, "opqdir"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(layerDir, "added"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(layerDir, ".wh.victim.txt"), nil, 0644))
	require.NoError(t, os.WriteFile(filepath.Join(layerDir, ".wh.removedir"), nil, 0644))
	require.NoError(t, os.WriteFile(filepath.Join(layerDir, "keep.txt"), []byte("new"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(layerDir, "opqdir", ".wh..wh..opq"), nil, 0644))
	require.NoError(t, os.WriteFile(filepath.Join(layerDir, "opqdir", "fresh.txt"), []byte("fresh"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(layerDir, "swapdir"), []byte("now a file"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(layerDir, "added", ".wh.foo"), nil, 0644))
	require.NoError(t, os.WriteFile(filepath.Join(layerDir, "added", "foo"), []byte("new"), 0644))

	require.NoError(t, applyLayerTree(layerDir, targetDir))

	// Whiteout removed the lower entry.
	_, err := os.Lstat(filepath.Join(targetDir, "victim.txt"))
	require.True(t, os.IsNotExist(err), "whiteout must delete the lower entry")
	_, err = os.Lstat(filepath.Join(targetDir, "removedir"))
	require.True(t, os.IsNotExist(err), "directory whiteout must delete the complete lower directory")

	// Regular file replacement.
	data, err := os.ReadFile(filepath.Join(targetDir, "keep.txt"))
	require.NoError(t, err)
	require.Equal(t, "new", string(data))

	// Opaque directory: stale content gone, layer content present.
	_, err = os.Lstat(filepath.Join(targetDir, "opqdir", "stale.txt"))
	require.True(t, os.IsNotExist(err), "opaque dir must hide lower contents")
	data, err = os.ReadFile(filepath.Join(targetDir, "opqdir", "fresh.txt"))
	require.NoError(t, err)
	require.Equal(t, "fresh", string(data))

	// Directory replaced by a file.
	info, err := os.Lstat(filepath.Join(targetDir, "swapdir"))
	require.NoError(t, err)
	require.False(t, info.IsDir())

	// Whiteout followed by recreate in the same layer keeps the new entry.
	data, err = os.ReadFile(filepath.Join(targetDir, "added", "foo"))
	require.NoError(t, err)
	require.Equal(t, "new", string(data))

	// Whiteout marker files never leak into the composed tree.
	leaked := make([]string, 0)
	require.NoError(t, filepath.WalkDir(targetDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), whiteoutPrefix) {
			leaked = append(leaked, path)
		}
		return nil
	}))
	require.Empty(t, leaked)
}

func TestUnpackLayerBlobIncludesTrailingTarPaddingInDiffID(t *testing.T) {
	root := t.TempDir()
	blobPath := filepath.Join(root, "layer.tar.gz")

	var tarData bytes.Buffer
	tw := tar.NewWriter(&tarData)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0644, Size: 1}))
	_, err := tw.Write([]byte("x"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	_, err = tarData.Write(make([]byte, 512))
	require.NoError(t, err)

	var compressed bytes.Buffer
	gzw := gzip.NewWriter(&compressed)
	_, err = gzw.Write(tarData.Bytes())
	require.NoError(t, err)
	require.NoError(t, gzw.Close())
	require.NoError(t, os.WriteFile(blobPath, compressed.Bytes(), 0644))

	stats, err := unpackLayerBlob(blobPath, "application/vnd.oci.image.layer.v1.tar+gzip", filepath.Join(root, "dest"))
	require.NoError(t, err)
	want := sha256.Sum256(tarData.Bytes())
	require.Equal(t, "sha256:"+fmt.Sprintf("%x", want), stats.diffID)
}

func TestUnpackLayerBlobRejectsSymlinkTraversal(t *testing.T) {
	root := t.TempDir()
	blobPath := filepath.Join(root, "layer.tar.gz")
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "outside"}))
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "link/escape", Typeflag: tar.TypeReg, Mode: 0644, Size: 1}))
	_, err := tw.Write([]byte("x"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())
	require.NoError(t, os.WriteFile(blobPath, buf.Bytes(), 0644))

	_, err = unpackLayerBlob(blobPath, "application/vnd.oci.image.layer.v1.tar+gzip", filepath.Join(root, "dest"))
	require.ErrorContains(t, err, "symlink")
}

func TestApplyLayerTreeSymlinksAndHardlinks(t *testing.T) {
	root := t.TempDir()
	targetDir := filepath.Join(root, "target")
	layerDir := filepath.Join(root, "layer")
	require.NoError(t, os.MkdirAll(targetDir, 0755))
	require.NoError(t, os.MkdirAll(layerDir, 0755))

	// A symlink in the lower tree pointing at a file the new layer deletes:
	// the symlink itself must be removed, never followed.
	require.NoError(t, os.WriteFile(filepath.Join(targetDir, "real.txt"), []byte("real"), 0644))
	require.NoError(t, os.Symlink("real.txt", filepath.Join(targetDir, "alias")))

	require.NoError(t, os.WriteFile(filepath.Join(layerDir, ".wh.alias"), nil, 0644))
	require.NoError(t, os.WriteFile(filepath.Join(layerDir, "a.txt"), []byte("content"), 0644))
	require.NoError(t, os.Link(filepath.Join(layerDir, "a.txt"), filepath.Join(layerDir, "b.txt")))
	require.NoError(t, os.Symlink("a.txt", filepath.Join(layerDir, "link-to-a")))

	require.NoError(t, applyLayerTree(layerDir, targetDir))

	_, err := os.Lstat(filepath.Join(targetDir, "alias"))
	require.True(t, os.IsNotExist(err), "symlink whiteout must remove the link itself")
	_, err = os.Lstat(filepath.Join(targetDir, "real.txt"))
	require.NoError(t, err, "symlink target must survive an unrelated whiteout")

	infoA, err := os.Stat(filepath.Join(targetDir, "a.txt"))
	require.NoError(t, err)
	infoB, err := os.Stat(filepath.Join(targetDir, "b.txt"))
	require.NoError(t, err)
	require.Equal(t, int64(7), infoA.Size())
	require.True(t, os.SameFile(infoA, infoB), "hardlinks within the layer must stay linked")

	linkTarget, err := os.Readlink(filepath.Join(targetDir, "link-to-a"))
	require.NoError(t, err)
	require.Equal(t, "a.txt", linkTarget)
}

func TestCopyXattrs(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	require.NoError(t, os.WriteFile(src, []byte("payload"), 0644))
	require.NoError(t, os.WriteFile(dst, []byte("payload"), 0644))

	err := unix.Lsetxattr(src, "user.one", []byte("1"), 0)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) {
		t.Skip("filesystem does not support user xattrs")
	}
	require.NoError(t, err)
	require.NoError(t, unix.Lsetxattr(src, "user.two", []byte("22"), 0))

	require.NoError(t, copyXattrs(src, dst))

	for name, want := range map[string]string{"user.one": "1", "user.two": "22"} {
		size, err := unix.Lgetxattr(dst, name, nil)
		require.NoError(t, err, "xattr %s must be copied", name)
		value := make([]byte, size)
		n, err := unix.Lgetxattr(dst, name, value)
		require.NoError(t, err)
		require.Equal(t, want, string(value[:n]))
	}
}

func TestUnpackLayerBlobPreservesDirMtime(t *testing.T) {
	root := t.TempDir()
	blobPath := filepath.Join(root, "layer.tar")
	dirTime := time.Now().Add(-time.Hour).Truncate(time.Second)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0755, ModTime: dirTime}))
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "d/file.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 1}))
	_, err := tw.Write([]byte("x"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, os.WriteFile(blobPath, buf.Bytes(), 0644))

	dest := filepath.Join(root, "dest")
	_, err = unpackLayerBlob(blobPath, "application/vnd.oci.image.layer.v1.tar", dest)
	require.NoError(t, err)

	info, err := os.Stat(filepath.Join(dest, "d"))
	require.NoError(t, err)
	require.True(t, info.ModTime().Equal(dirTime), "dir mtime must come from the tar header")
}

func TestApplyLayerTreePreservesDirMtime(t *testing.T) {
	root := t.TempDir()
	targetDir := filepath.Join(root, "target")
	layerDir := filepath.Join(root, "layer")

	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	require.NoError(t, os.MkdirAll(filepath.Join(layerDir, "d"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(layerDir, "d", "file.txt"), []byte("x"), 0644))
	require.NoError(t, os.Chtimes(filepath.Join(layerDir, "d"), old, old))

	require.NoError(t, applyLayerTree(layerDir, targetDir))

	info, err := os.Stat(filepath.Join(targetDir, "d"))
	require.NoError(t, err)
	require.True(t, info.ModTime().Equal(old), "dir mtime must survive the merge")
}
