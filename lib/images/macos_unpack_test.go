package images

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	gcr "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/stretchr/testify/require"
)

func machineTestLayer(t *testing.T, entries ...tar.Header) gcr.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		data := make([]byte, h.Size)
		if h.Typeflag == tar.TypeReg && h.Size > 0 {
			copy(data, "head")
			copy(data[len(data)-4:], "tail")
		}
		require.NoError(t, tw.WriteHeader(&h))
		_, err := tw.Write(data)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf.Bytes())), nil })
	require.NoError(t, err)
	return layer
}

// allocatedBytes reports the bytes a file occupies on disk.
func allocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Sys().(*syscall.Stat_t).Blocks * 512
}

// sparseControlBytes reports what a file with data only at its ends occupies in
// dir. Filesystems differ in which holes they keep, so sparseness is checked
// against this control rather than a fixed ratio.
func sparseControlBytes(t *testing.T, dir string, size int64) int64 {
	t.Helper()
	path := filepath.Join(dir, "sparse-control")
	file, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(size))
	_, err = file.WriteAt([]byte("head"), 0)
	require.NoError(t, err)
	_, err = file.WriteAt([]byte("tail"), size-4)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	allocated := allocatedBytes(t, path)
	require.NoError(t, os.Remove(path))
	if allocated >= size/2 {
		t.Skipf("filesystem does not keep holes in a %d-byte file", size)
	}
	return allocated
}

func TestUnpackMachineLayerWritesSparseFiles(t *testing.T) {
	size := int64(64 << 20)
	layer := machineTestLayer(t, tar.Header{Name: "bundle/", Typeflag: tar.TypeDir, Mode: 0755}, tar.Header{Name: "bundle/disk.img", Typeflag: tar.TypeReg, Mode: 0644, Size: size})
	diffID, err := layer.DiffID()
	require.NoError(t, err)
	target := t.TempDir()
	require.NoError(t, unpackMachineLayer(context.Background(), layer, diffID, target))

	path := filepath.Join(target, "bundle", "disk.img")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Len(t, data, int(size))
	require.Equal(t, "head", string(data[:4]))
	require.Equal(t, "tail", string(data[size-4:]))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	control := sparseControlBytes(t, target, size)
	require.LessOrEqual(t, allocatedBytes(t, path), control+2*machineUnpackChunk, "zero chunks must become holes")
}

func TestUnpackMachineLayerRejectsUnsafeEntries(t *testing.T) {
	for name, entry := range map[string]tar.Header{
		"symlink":  {Name: "disk.img", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		"hardlink": {Name: "disk.img", Typeflag: tar.TypeLink, Linkname: "other"},
		"escape":   {Name: "../disk.img", Typeflag: tar.TypeReg, Size: 4},
		"absolute": {Name: "/disk.img", Typeflag: tar.TypeReg, Size: 4},
		"whiteout": {Name: ".wh.disk.img", Typeflag: tar.TypeReg},
	} {
		t.Run(name, func(t *testing.T) {
			layer := machineTestLayer(t, entry)
			diffID, err := layer.DiffID()
			require.NoError(t, err)
			parent := t.TempDir()
			target := filepath.Join(parent, "rootfs")
			require.NoError(t, os.Mkdir(target, 0700))
			require.Error(t, unpackMachineLayer(context.Background(), layer, diffID, target))
			_, err = os.Lstat(filepath.Join(parent, "disk.img"))
			require.True(t, os.IsNotExist(err), "nothing is written outside the target")
		})
	}
}

func TestUnpackMachineLayerVerifiesDiffID(t *testing.T) {
	layer := machineTestLayer(t, tar.Header{Name: "disk.img", Typeflag: tar.TypeReg, Size: 16})
	other := machineTestLayer(t, tar.Header{Name: "disk.img", Typeflag: tar.TypeReg, Size: 32})
	wrong, err := other.DiffID()
	require.NoError(t, err)
	require.ErrorContains(t, unpackMachineLayer(context.Background(), layer, wrong, t.TempDir()), "does not match diff ID")
}
