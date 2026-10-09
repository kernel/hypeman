package ocicache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/stretchr/testify/require"
)

func TestAppendImageConcurrent(t *testing.T) {
	cacheDir := t.TempDir()
	const count = 64
	images := make([]v1.Image, count)
	for i := range images {
		var err error
		images[i], err = random.Image(128, 1)
		require.NoError(t, err)
	}

	start := make(chan struct{})
	errs := make(chan error, count)
	var writers sync.WaitGroup
	for i, img := range images {
		writers.Add(1)
		go func() {
			defer writers.Done()
			<-start
			errs <- AppendImage(cacheDir, img, fmt.Sprintf("image-%d", i))
		}()
	}
	close(start)
	writers.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	manifest := readIndex(t, cacheDir)
	require.Len(t, manifest.Manifests, count)
	tags := make(map[string]v1.Hash, count)
	for _, desc := range manifest.Manifests {
		tags[desc.Annotations["org.opencontainers.image.ref.name"]] = desc.Digest
	}
	for i, img := range images {
		digest, err := img.Digest()
		require.NoError(t, err)
		require.Equal(t, digest, tags[fmt.Sprintf("image-%d", i)])
		requireImageReadable(t, cacheDir, digest)
	}
	requireNoTempFiles(t, cacheDir)
}

func TestAppendImageSameImageConcurrent(t *testing.T) {
	cacheDir := t.TempDir()
	img, err := random.Image(128, 2)
	require.NoError(t, err)
	digest, err := img.Digest()
	require.NoError(t, err)

	const count = 16
	start := make(chan struct{})
	errs := make(chan error, count)
	var writers sync.WaitGroup
	for i := range count {
		writers.Add(1)
		go func() {
			defer writers.Done()
			<-start
			errs <- AppendImage(cacheDir, img, fmt.Sprintf("tag-%d", i))
		}()
	}
	close(start)
	writers.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	manifest := readIndex(t, cacheDir)
	require.Len(t, manifest.Manifests, count)
	for _, desc := range manifest.Manifests {
		require.Equal(t, digest, desc.Digest)
	}
	requireImageReadable(t, cacheDir, digest)
	requireNoTempFiles(t, cacheDir)
}

func TestAppendImageRewritesTruncatedBlob(t *testing.T) {
	cacheDir := t.TempDir()
	img, err := random.Image(128, 1)
	require.NoError(t, err)
	require.NoError(t, AppendImage(cacheDir, img, "first"))

	cfgName, err := img.ConfigName()
	require.NoError(t, err)
	cfgPath := filepath.Join(cacheDir, "blobs", cfgName.Algorithm, cfgName.Hex)
	require.NoError(t, os.WriteFile(cfgPath, []byte("{"), 0o644))

	require.NoError(t, AppendImage(cacheDir, img, "second"))
	cfg, err := img.RawConfigFile()
	require.NoError(t, err)
	got, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	require.Equal(t, cfg, got)
}

func TestAppendImageAtomicIndexReaders(t *testing.T) {
	cacheDir := t.TempDir()
	img, err := random.Image(128, 1)
	require.NoError(t, err)
	for i := range 200 {
		require.NoError(t, AppendImage(cacheDir, img, fmt.Sprintf("seed-%d", i)))
	}

	done := make(chan struct{})
	readerErr := make(chan error, 1)
	go func() {
		indexPath := filepath.Join(cacheDir, "index.json")
		for {
			select {
			case <-done:
				readerErr <- nil
				return
			default:
			}
			data, err := os.ReadFile(indexPath)
			if err == nil && !json.Valid(data) {
				err = fmt.Errorf("reader observed an invalid index")
			}
			if err != nil {
				readerErr <- err
				return
			}
		}
	}()
	for i := range 64 {
		if err := AppendImage(cacheDir, img, fmt.Sprintf("tag-%d", i)); err != nil {
			t.Error(err)
			break
		}
	}
	close(done)
	require.NoError(t, <-readerErr)
}

func readIndex(t *testing.T, cacheDir string) *v1.IndexManifest {
	t.Helper()
	path, err := layout.FromPath(cacheDir)
	require.NoError(t, err)
	index, err := path.ImageIndex()
	require.NoError(t, err)
	manifest, err := index.IndexManifest()
	require.NoError(t, err)
	return manifest
}

func requireImageReadable(t *testing.T, cacheDir string, digest v1.Hash) {
	t.Helper()
	path, err := layout.FromPath(cacheDir)
	require.NoError(t, err)
	img, err := path.Image(digest)
	require.NoError(t, err)
	_, err = img.ConfigFile()
	require.NoError(t, err)
	layers, err := img.Layers()
	require.NoError(t, err)
	for _, layer := range layers {
		size, err := layer.Size()
		require.NoError(t, err)
		digest, err := layer.Digest()
		require.NoError(t, err)
		info, err := os.Stat(filepath.Join(cacheDir, "blobs", digest.Algorithm, digest.Hex))
		require.NoError(t, err)
		require.Equal(t, size, info.Size())
	}
}

func requireNoTempFiles(t *testing.T, cacheDir string) {
	t.Helper()
	for _, pattern := range []string{".*.tmp-*", "blobs/sha256/.*.tmp-*"} {
		files, err := filepath.Glob(filepath.Join(cacheDir, pattern))
		require.NoError(t, err)
		require.Empty(t, files)
	}
}
