package ocicache

import (
	"fmt"
	"sync"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/partial"
)

// indexMu serializes index.json updates. layout.AppendDescriptor does an
// unsynchronized read-modify-write of index.json, so concurrent appends lose
// descriptors or corrupt the file.
var indexMu sync.Mutex

// AppendImage writes img's blobs to the OCI layout at cacheDir, then appends a
// descriptor tagged with org.opencontainers.image.ref.name=tag to index.json.
// Blob writes run concurrently; only the index update is serialized.
func AppendImage(cacheDir string, img v1.Image, tag string) error {
	path := layout.Path(cacheDir)
	if err := path.WriteImage(img); err != nil {
		return fmt.Errorf("write image blobs: %w", err)
	}
	desc, err := partial.Descriptor(img)
	if err != nil {
		return fmt.Errorf("describe image: %w", err)
	}
	if desc.Annotations == nil {
		desc.Annotations = make(map[string]string)
	}
	desc.Annotations["org.opencontainers.image.ref.name"] = tag

	indexMu.Lock()
	defer indexMu.Unlock()

	if _, err := layout.FromPath(cacheDir); err != nil {
		if _, err := layout.Write(cacheDir, empty.Index); err != nil {
			return fmt.Errorf("create OCI layout: %w", err)
		}
	}
	return path.AppendDescriptor(*desc)
}
