package images

import (
	"sort"

	"github.com/kernel/hypeman/lib/paths"
)

// LiveOCICacheDigests returns the content digests that image metadata keeps
// alive in the shared OCI cache: the manifest digest and every recorded layer
// digest of each non-failed image. Failed images contribute nothing so their
// blobs become collectable once nothing else roots them.
//
// In-flight builds are protected because pending/pulling/converting metadata
// already carries the manifest digest as soon as the build is queued, and
// layer digests recorded at finalize keep blobs alive even after the image is
// no longer rooted in the OCI layout index.
func LiveOCICacheDigests(p *paths.Paths) []string {
	metas, err := listAllMetadata(p)
	if err != nil {
		return nil
	}

	seen := make(map[string]struct{})
	out := make([]string, 0)
	add := func(digest string) {
		if digest == "" {
			return
		}
		if _, ok := seen[digest]; ok {
			return
		}
		seen[digest] = struct{}{}
		out = append(out, digest)
	}
	for _, meta := range metas {
		if meta.Status == StatusFailed {
			continue
		}
		add(meta.Digest)
		for _, layer := range meta.Layers {
			add(layer.Digest)
		}
	}
	sort.Strings(out)
	return out
}

// OCICacheRoots exposes image metadata as extra roots for the OCI cache
// garbage collector (lib/ocicachegc). It satisfies that package's
// RootsProvider interface structurally so the images package does not need
// to import it.
type OCICacheRoots struct {
	paths *paths.Paths
}

func NewOCICacheRoots(p *paths.Paths) OCICacheRoots {
	return OCICacheRoots{paths: p}
}

// LiveCacheManifestDigests returns the manifest and layer digests kept alive
// by image metadata. Layer digests are not manifests; the collector marks
// them live and treats their blobs as opaque leaves.
func (r OCICacheRoots) LiveCacheManifestDigests() []string {
	return LiveOCICacheDigests(r.paths)
}

// layerAccounting summarises the bytes of OCI layers referenced by image
// metadata. Each layer digest is counted once regardless of how many images
// reference it, so shared layers are not double-counted.
type layerAccounting struct {
	// uniqueBytes counts every referenced layer once.
	uniqueBytes int64
	// sharedBytes is the subset of uniqueBytes referenced by more than one
	// image.
	sharedBytes int64
}

func computeLayerAccounting(metas []*imageMetadata) layerAccounting {
	type layerEntry struct {
		size int64
		refs int
	}
	layers := make(map[string]*layerEntry)
	for _, meta := range metas {
		if meta.Status == StatusFailed {
			continue
		}
		seenInImage := make(map[string]struct{}, len(meta.Layers))
		for _, layer := range meta.Layers {
			if layer.Digest == "" {
				continue
			}
			if _, dup := seenInImage[layer.Digest]; dup {
				continue
			}
			seenInImage[layer.Digest] = struct{}{}
			entry, ok := layers[layer.Digest]
			if !ok {
				entry = &layerEntry{size: layer.Size}
				layers[layer.Digest] = entry
			}
			entry.refs++
		}
	}

	var accounting layerAccounting
	for _, entry := range layers {
		accounting.uniqueBytes += entry.size
		if entry.refs > 1 {
			accounting.sharedBytes += entry.size
		}
	}
	return accounting
}
