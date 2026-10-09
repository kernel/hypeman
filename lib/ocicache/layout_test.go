package ocicache

import (
	"fmt"
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

	path, err := layout.FromPath(cacheDir)
	require.NoError(t, err)
	index, err := path.ImageIndex()
	require.NoError(t, err)
	manifest, err := index.IndexManifest()
	require.NoError(t, err)
	require.Len(t, manifest.Manifests, count)
	tags := make(map[string]v1.Hash, count)
	for _, desc := range manifest.Manifests {
		tags[desc.Annotations["org.opencontainers.image.ref.name"]] = desc.Digest
	}
	for i, img := range images {
		digest, err := img.Digest()
		require.NoError(t, err)
		require.Equal(t, digest, tags[fmt.Sprintf("image-%d", i)])
		cached, err := path.Image(digest)
		require.NoError(t, err)
		_, err = cached.ConfigFile()
		require.NoError(t, err)
	}
}
