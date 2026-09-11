package images

import (
	"sync"

	"github.com/kernel/hypeman/lib/paths"
	"golang.org/x/sync/singleflight"
)

// layerStore owns the state shared by layer materialization and accounting.
// Keeping it separate from manager makes the layer-specific concurrency and
// cache lifecycle explicit.
type layerStore struct {
	paths               *paths.Paths
	flights             singleflight.Group
	builds              chan struct{}
	artifactsSupported  bool
	diskUsageMu         sync.RWMutex
	diskUsageGeneration uint64
	activeBuilds        int
	diskUsageLoaded     bool
	readyImageBytes     int64
	ociCacheBytes       int64
}

func newLayerStore(p *paths.Paths, maxConcurrentBuilds int) *layerStore {
	if maxConcurrentBuilds < 1 {
		maxConcurrentBuilds = 1
	}
	return &layerStore{
		paths:              p,
		builds:             make(chan struct{}, maxConcurrentBuilds),
		artifactsSupported: probeLayerArtifactSupport(p.ImageLayersDir()),
	}
}
