package images

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
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
	require.NoError(t, unpackMachineLayer(context.Background(), layer, diffID, target, unlimitedMachineBudget()))

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
			require.Error(t, unpackMachineLayer(context.Background(), layer, diffID, target, unlimitedMachineBudget()))
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
	require.ErrorContains(t, unpackMachineLayer(context.Background(), layer, wrong, t.TempDir(), unlimitedMachineBudget()), "does not match diff ID")
}

func unlimitedMachineBudget() *int64 {
	remaining := maxMachineUnpackedBytes
	return &remaining
}

func TestUnpackMachineLayerEnforcesUnpackedSizeLimit(t *testing.T) {
	const budget = 1 << 20
	layerFromBytes := func(data []byte) gcr.Layer {
		layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil })
		require.NoError(t, err)
		return layer
	}
	unpack := func(layer gcr.Layer, target string, remaining *int64) error {
		diffID, err := layer.DiffID()
		require.NoError(t, err)
		return unpackMachineLayer(context.Background(), layer, diffID, target, remaining)
	}

	t.Run("declared size over the limit", func(t *testing.T) {
		remaining := int64(budget)
		target := t.TempDir()
		err := unpack(machineTestLayer(t, tar.Header{Name: "disk.img", Typeflag: tar.TypeReg, Size: 2 * budget}), target, &remaining)
		require.ErrorContains(t, err, "unpacked size limit")
		require.NoFileExists(t, filepath.Join(target, "disk.img"), "no file is sized before the check")
	})

	t.Run("data after the end of the archive", func(t *testing.T) {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: "aux.img", Typeflag: tar.TypeReg, Size: 4}))
		_, err := tw.Write([]byte("data"))
		require.NoError(t, err)
		require.NoError(t, tw.Close())
		buf.Write(bytes.Repeat([]byte{0xff}, 2*budget))
		remaining := int64(budget)
		require.ErrorContains(t, unpack(layerFromBytes(buf.Bytes()), t.TempDir(), &remaining), "unpacked size limit")
	})

	t.Run("budget is shared across layers", func(t *testing.T) {
		remaining := int64(budget)
		target := t.TempDir()
		half := int64(budget * 6 / 10)
		require.NoError(t, unpack(machineTestLayer(t, tar.Header{Name: "disk.img", Typeflag: tar.TypeReg, Size: half}), target, &remaining))
		require.Less(t, remaining, int64(budget-half), "the first layer spent its bytes")
		require.ErrorContains(t, unpack(machineTestLayer(t, tar.Header{Name: "aux.img", Typeflag: tar.TypeReg, Size: half}), target, &remaining), "unpacked size limit")
	})
}

// paxGNUSparseArchive returns count PAX GNU sparse 1.0 entries, each a 4-byte file
// that the tar reader expands to size bytes of mostly holes. archive/tar cannot write
// sparse entries, so each PAX header is written as a regular entry and retyped.
func paxGNUSparseArchive(t *testing.T, count int, size int64) []byte {
	t.Helper()
	record := func(k, v string) string {
		n := len(k) + len(v) + 3
		for {
			s := fmt.Sprintf("%d %s=%s\n", n, k, v)
			if len(s) == n {
				return s
			}
			n = len(s)
		}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i := range count {
		name := fmt.Sprintf("disk-%d.img", i)
		pax := record("GNU.sparse.major", "1") + record("GNU.sparse.minor", "0") + record("GNU.sparse.name", name) + record("GNU.sparse.realsize", strconv.FormatInt(size, 10))
		require.NoError(t, tw.Flush())
		start := buf.Len()
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: "PaxHeaders/" + name, Typeflag: tar.TypeReg, Size: int64(len(pax)), Format: tar.FormatUSTAR}))
		_, err := tw.Write([]byte(pax))
		require.NoError(t, err)
		require.NoError(t, tw.Flush())
		header := buf.Bytes()[start : start+512]
		header[156] = tar.TypeXHeader
		copy(header[148:156], "        ")
		sum := 0
		for _, b := range header {
			sum += int(b)
		}
		copy(header[148:156], fmt.Sprintf("%06o\x00 ", sum))
		// Sparse 1.0 data starts with the map, padded to a block: one 4-byte region at 0.
		body := make([]byte, 512, 516)
		copy(body, "1\n0\n4\n")
		body = append(body, "data"...)
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: "GNUSparseFile.0/" + name, Typeflag: tar.TypeReg, Size: int64(len(body)), Format: tar.FormatUSTAR}))
		_, err = tw.Write(body)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

func TestUnpackMachineLayerRejectsGNUSparseEntries(t *testing.T) {
	data := paxGNUSparseArchive(t, 16, 512<<10)
	reader := tar.NewReader(bytes.NewReader(data))
	header, err := reader.Next()
	require.NoError(t, err)
	require.True(t, isGNUSparse(header), "the fixture must read back as a sparse entry")
	require.EqualValues(t, 512<<10, header.Size)

	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil })
	require.NoError(t, err)
	diffID, err := layer.DiffID()
	require.NoError(t, err)
	remaining := int64(1 << 20)
	target := t.TempDir()
	require.ErrorContains(t, unpackMachineLayer(context.Background(), layer, diffID, target, &remaining), "GNU sparse")
	entries, err := os.ReadDir(target)
	require.NoError(t, err)
	require.Empty(t, entries, "no sparse entry is expanded")
}
