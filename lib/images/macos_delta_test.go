package images

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	gcr "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/stretchr/testify/require"
)

type machineTestFile struct {
	name string
	data []byte
}

// machineFilesLayer builds a layer of regular files with the given media type.
func machineFilesLayer(t *testing.T, mediaType types.MediaType, files ...machineTestFile) gcr.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0600, Size: int64(len(f.data)), Typeflag: tar.TypeReg}))
		_, err := tw.Write(f.data)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf.Bytes())), nil }, tarball.WithMediaType(mediaType))
	require.NoError(t, err)
	return layer
}

// deltaTestDisk returns a disk of n chunks, each filled with a distinct byte.
func deltaTestDisk(n int) []byte {
	disk := make([]byte, n*MacOSDeltaChunkSize)
	for i := range n {
		for j := range MacOSDeltaChunkSize {
			disk[i*MacOSDeltaChunkSize+j] = byte(i + 1)
		}
	}
	return disk
}

func writeDeltaTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, data, 0600))
}

// deltaTestLayer builds a delta layer from a manifest and a bundle directory.
func deltaTestLayer(t *testing.T, delta MacOSDelta, derived, bundle string) gcr.Layer {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, WriteMacOSDeltaTar(&buf, delta, derived, bundle))
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf.Bytes())), nil }, tarball.WithMediaType(MacOSDeltaMediaType))
	require.NoError(t, err)
	return layer
}

func TestMacOSDeltaRoundTripAppliesOnlyChangedChunks(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := deltaTestDisk(8)
	derived := append([]byte(nil), base...)
	for j := range MacOSDeltaChunkSize {
		derived[2*MacOSDeltaChunkSize+j] = 0xAA
		derived[5*MacOSDeltaChunkSize+j] = 0
	}
	basePath := filepath.Join(dir, "base-disk.img")
	derivedPath := filepath.Join(dir, "derived-disk.img")
	writeDeltaTestFile(t, basePath, base)
	writeDeltaTestFile(t, derivedPath, derived)
	bundle := filepath.Join(dir, "bundle")
	require.NoError(t, os.Mkdir(bundle, 0700))
	writeDeltaTestFile(t, filepath.Join(bundle, "aux.img"), []byte("derived aux"))
	writeDeltaTestFile(t, filepath.Join(bundle, "config.json"), []byte(`{"derived":true}`))

	delta, err := ComputeMacOSDelta(basePath, derivedPath)
	require.NoError(t, err)
	require.Equal(t, []int64{2, 5}, delta.Chunks)

	baseLayer := machineFilesLayer(t, types.OCILayer,
		machineTestFile{name: "disk.img", data: base},
		machineTestFile{name: "aux.img", data: []byte("base aux")},
		machineTestFile{name: "config.json", data: []byte(`{"base":true}`)},
	)
	deltaLayer := deltaTestLayer(t, delta, derivedPath, bundle)
	target := t.TempDir()
	require.NoError(t, unpackMachineLayers2(ctx, target, baseLayer, deltaLayer))

	got, err := os.ReadFile(filepath.Join(target, "disk.img"))
	require.NoError(t, err)
	require.Equal(t, derived, got)
	aux, err := os.ReadFile(filepath.Join(target, "aux.img"))
	require.NoError(t, err)
	require.Equal(t, "derived aux", string(aux))
	config, err := os.ReadFile(filepath.Join(target, "config.json"))
	require.NoError(t, err)
	require.Equal(t, `{"derived":true}`, string(config))
}

func TestMacOSDeltaRejectsBaseOfDifferentSize(t *testing.T) {
	dir := t.TempDir()
	bundle := writeDeltaBundle(t, dir)
	derivedPath := filepath.Join(dir, "derived-disk.img")
	writeDeltaTestFile(t, derivedPath, deltaTestDisk(8))
	delta := MacOSDelta{Version: 1, ChunkSize: MacOSDeltaChunkSize, Size: 8 * MacOSDeltaChunkSize, Chunks: []int64{1}}
	baseLayer := machineFilesLayer(t, types.OCILayer, machineTestFile{name: "disk.img", data: deltaTestDisk(4)})
	err := unpackMachineLayers2(context.Background(), t.TempDir(), baseLayer, deltaTestLayer(t, delta, derivedPath, bundle))
	require.ErrorContains(t, err, "base disk does not match")
}

func TestMacOSDeltaRejectsLayerWithoutBase(t *testing.T) {
	dir := t.TempDir()
	bundle := writeDeltaBundle(t, dir)
	derivedPath := filepath.Join(dir, "derived-disk.img")
	writeDeltaTestFile(t, derivedPath, deltaTestDisk(8))
	delta := MacOSDelta{Version: 1, ChunkSize: MacOSDeltaChunkSize, Size: 8 * MacOSDeltaChunkSize, Chunks: []int64{1}}
	err := unpackMachineLayers2(context.Background(), t.TempDir(), deltaTestLayer(t, delta, derivedPath, bundle))
	require.ErrorContains(t, err, "delta has no base disk")
}

func TestMacOSDeltaRejectsIncompleteAndMisorderedLayers(t *testing.T) {
	ctx := context.Background()
	manifest := []byte(`{"version":1,"chunk_size":65536,"size":524288,"chunks":[]}`)
	cases := map[string][]machineTestFile{
		"missing entries": {{name: "delta.json", data: manifest}},
		"wrong first entry": {
			{name: "aux.img", data: []byte("x")},
		},
		"patch skipped": {
			{name: "delta.json", data: manifest},
			{name: "aux.img", data: []byte("x")},
		},
		"extra entry": {
			{name: "delta.json", data: manifest},
			{name: "disk.img.patch"},
			{name: "aux.img", data: []byte("x")},
			{name: "config.json", data: []byte("{}")},
			{name: "extra", data: []byte("x")},
		},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			target := t.TempDir()
			writeDeltaTestFile(t, filepath.Join(target, "disk.img"), deltaTestDisk(8))
			layer := machineFilesLayer(t, MacOSDeltaMediaType, files...)
			diffID, err := layer.DiffID()
			require.NoError(t, err)
			require.Error(t, unpackMachineLayer(ctx, layer, diffID, target, unlimitedMachineBudget()))
		})
	}
}

func TestMacOSDeltaValidateRejectsBadManifests(t *testing.T) {
	size := int64(8 * MacOSDeltaChunkSize)
	cases := map[string]MacOSDelta{
		"version":      {Version: 2, ChunkSize: MacOSDeltaChunkSize, Size: size},
		"chunk size":   {Version: 1, ChunkSize: 4096, Size: size},
		"partial size": {Version: 1, ChunkSize: MacOSDeltaChunkSize, Size: size + 1},
		"unordered":    {Version: 1, ChunkSize: MacOSDeltaChunkSize, Size: size, Chunks: []int64{3, 1}},
		"duplicate":    {Version: 1, ChunkSize: MacOSDeltaChunkSize, Size: size, Chunks: []int64{3, 3}},
		"out of range": {Version: 1, ChunkSize: MacOSDeltaChunkSize, Size: size, Chunks: []int64{8}},
	}
	for name, delta := range cases {
		t.Run(name, func(t *testing.T) {
			require.Error(t, delta.Validate())
		})
	}
	valid := MacOSDelta{Version: 1, ChunkSize: MacOSDeltaChunkSize, Size: size, Chunks: []int64{0, 7}}
	require.NoError(t, valid.Validate())
}

// writeDeltaBundle writes the aux and config files a delta layer copies in full.
func writeDeltaBundle(t *testing.T, dir string) string {
	t.Helper()
	bundle := filepath.Join(dir, "bundle")
	require.NoError(t, os.Mkdir(bundle, 0700))
	writeDeltaTestFile(t, filepath.Join(bundle, "aux.img"), []byte("aux"))
	writeDeltaTestFile(t, filepath.Join(bundle, "config.json"), []byte("{}"))
	return bundle
}

// unpackMachineLayers2 unpacks layers in order into one target directory with an
// unlimited budget, as the bundle unpack does.
func unpackMachineLayers2(ctx context.Context, target string, layers ...gcr.Layer) error {
	budget := unlimitedMachineBudget()
	for _, l := range layers {
		diffID, err := l.DiffID()
		if err != nil {
			return err
		}
		if err := unpackMachineLayer(ctx, l, diffID, target, budget); err != nil {
			return err
		}
	}
	return nil
}
