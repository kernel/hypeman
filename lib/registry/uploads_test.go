package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

// randomBlob is an incompressible blob generated on demand, so a large blob needs
// no memory to push.
type randomBlob struct {
	size   int64
	digest v1.Hash
}

func newRandomBlob(t *testing.T, size int64) *randomBlob {
	b := &randomBlob{size: size}
	hash := sha256.New()
	body, err := b.Compressed()
	require.NoError(t, err)
	_, err = io.Copy(hash, body)
	require.NoError(t, err)
	b.digest = v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(hash.Sum(nil))}
	return b
}

func (b *randomBlob) Compressed() (io.ReadCloser, error) {
	return io.NopCloser(io.LimitReader(rand.NewChaCha8([32]byte{}), b.size)), nil
}
func (b *randomBlob) Uncompressed() (io.ReadCloser, error) { return b.Compressed() }
func (b *randomBlob) Digest() (v1.Hash, error)             { return b.digest, nil }
func (b *randomBlob) DiffID() (v1.Hash, error)             { return b.digest, nil }
func (b *randomBlob) Size() (int64, error)                 { return b.size, nil }
func (b *randomBlob) MediaType() (types.MediaType, error)  { return types.OCILayer, nil }

// TestChunkedUploadStreamsToDisk pushes a layer far larger than the heap bound
// through go-containerregistry's chunked upload, and checks it lands in the blob
// store without being held in memory.
func TestChunkedUploadStreamsToDisk(t *testing.T) {
	const size = 768 << 20
	p := paths.New(t.TempDir())
	reg, err := New(p, nil)
	require.NoError(t, err)
	server := httptest.NewServer(reg.Handler())
	defer server.Close()

	layer := newRandomBlob(t, size)
	ref, err := name.ParseReference(strings.TrimPrefix(server.URL, "http://")+"/cache/big:latest", name.Insecure)
	require.NoError(t, err)

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	done := make(chan struct{})
	go func() {
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapInuse > peak.Load() {
				peak.Store(m.HeapInuse)
			}
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	err = remote.WriteLayer(ref.Context(), layer, remote.WithContext(ctx))
	close(done)
	require.NoError(t, err)

	stored, err := reg.blobStore.Stat(ctx, "cache/big", layer.digest)
	require.NoError(t, err)
	require.Equal(t, int64(size), stored)
	grew := int64(peak.Load()) - int64(base.HeapInuse)
	t.Logf("pushed %d MiB; heap grew %d MiB", size>>20, grew>>20)
	require.Less(t, grew, int64(64<<20), "the upload is not buffered in memory")
}

func TestChunkedUploadRejectsBadChunks(t *testing.T) {
	reg, err := New(paths.New(t.TempDir()), nil)
	require.NoError(t, err)
	server := httptest.NewServer(reg.Handler())
	defer server.Close()
	do := func(method, url, contentRange string, body string) *http.Response {
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		require.NoError(t, err)
		if contentRange != "" {
			req.Header.Set("Content-Range", contentRange)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp
	}
	start := func() string {
		resp := do(http.MethodPost, server.URL+"/v2/repo/blobs/uploads/", "", "")
		require.Equal(t, http.StatusAccepted, resp.StatusCode)
		return server.URL + resp.Header.Get("Location")
	}
	sum := sha256.Sum256([]byte("abcdef"))
	digest := "sha256:" + hex.EncodeToString(sum[:])

	upload := start()
	require.Equal(t, http.StatusAccepted, do(http.MethodPatch, upload, "0-2", "abc").StatusCode)
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, do(http.MethodPatch, upload, "0-2", "def").StatusCode, "a chunk must continue the upload")
	require.Equal(t, http.StatusAccepted, do(http.MethodPatch, upload, "3-5", "def").StatusCode)
	require.Equal(t, http.StatusCreated, do(http.MethodPut, upload+"?digest="+digest, "", "").StatusCode)
	require.Equal(t, http.StatusNotFound, do(http.MethodPatch, upload, "", "x").StatusCode, "a committed upload is closed")

	other := sha256.Sum256([]byte("xyz"))
	upload = start()
	require.Equal(t, http.StatusBadRequest, do(http.MethodPut, upload+"?digest=sha256:"+hex.EncodeToString(other[:]), "", "xyZ").StatusCode)
	_, err = reg.blobStore.Stat(context.Background(), "repo", v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(other[:])})
	require.ErrorIs(t, err, ErrNotFound, "a mismatched upload is not stored")
	entries, err := os.ReadDir(reg.uploads.dir)
	require.NoError(t, err)
	require.Empty(t, entries, "ended uploads leave no files")
}
