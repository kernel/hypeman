package images

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"testing"

	gcr "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/stretchr/testify/require"
)

type tarEntrySpec struct {
	name    string
	content string
	isDir   bool
	mode    int64
}

// specLayer builds a gzipped tar layer from entry specs in order.
func specLayer(t *testing.T, entries []tarEntrySpec) gcr.Layer {
	t.Helper()

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for _, entry := range entries {
		if entry.isDir {
			require.NoError(t, tw.WriteHeader(&tar.Header{Name: entry.name, Typeflag: tar.TypeDir, Mode: entry.mode}))
			continue
		}
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name:     entry.name,
			Typeflag: tar.TypeReg,
			Mode:     entry.mode,
			Size:     int64(len(entry.content)),
		}))
		_, err := tw.Write([]byte(entry.content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())

	data := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	require.NoError(t, err)
	return layer
}
