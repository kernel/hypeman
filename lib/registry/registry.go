// Package registry implements an OCI Distribution Spec registry that accepts pushed images
// and triggers conversion to hypeman's disk format.
package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/ocicache"
	"github.com/kernel/hypeman/lib/paths"
)

// Registry provides an OCI Distribution Spec compliant registry that stores pushed images
// in hypeman's OCI cache and triggers conversion to ext4 disk format.
type Registry struct {
	paths        *paths.Paths
	imageManager images.Manager
	blobStore    *BlobStore
	handler      http.Handler

	// cacheTags tracks BuildKit cache repo tags ("cache/<...>:<reference>")
	// to their manifest digest. The underlying go-containerregistry registry
	// keeps tags in memory and they are not rooted in the OCI cache
	// index.json, so without this map the OCI cache GC has no way to mark
	// the manifest, config, and layer blobs that BuildKit cache exports
	// rely on. Cleared with the process; tags do not survive restart.
	cacheTagsMu sync.RWMutex
	cacheTags   map[string]string
}

// manifestPutPattern matches PUT requests to /v2/{name}/manifests/{reference}
var manifestPutPattern = regexp.MustCompile(`^/v2/(.+)/manifests/(.+)$`)

// New creates a new Registry that stores blobs in the OCI cache directory
// and triggers image conversion when manifests are pushed.
func New(p *paths.Paths, imgManager images.Manager) (*Registry, error) {
	blobStore, err := NewBlobStore(p)
	if err != nil {
		return nil, err
	}

	// Create registry with custom blob handler
	regHandler := registry.New(
		registry.WithBlobHandler(blobStore),
	)

	r := &Registry{
		paths:        p,
		imageManager: imgManager,
		blobStore:    blobStore,
		handler:      regHandler,
		cacheTags:    make(map[string]string),
	}

	return r, nil
}

// LiveCacheManifestDigests returns the manifest digests of every BuildKit
// cache tag the registry has accepted since startup. Used by the OCI cache
// GC as additional roots: the in-memory registry never adds these to
// index.json, so without these roots the GC would sweep cache blobs that
// are still being served to BuildKit clients.
func (r *Registry) LiveCacheManifestDigests() []string {
	r.cacheTagsMu.RLock()
	defer r.cacheTagsMu.RUnlock()
	if len(r.cacheTags) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(r.cacheTags))
	out := make([]string, 0, len(r.cacheTags))
	for _, digest := range r.cacheTags {
		if _, ok := seen[digest]; ok {
			continue
		}
		seen[digest] = struct{}{}
		out = append(out, digest)
	}
	return out
}

// recordCacheTag stores the (repo, reference) -> digest mapping for a
// BuildKit cache push. Replaces any prior digest for the same tag.
func (r *Registry) recordCacheTag(repo, reference, digest string) {
	r.cacheTagsMu.Lock()
	defer r.cacheTagsMu.Unlock()
	r.cacheTags[repo+":"+reference] = digest
}

// isCacheRepo reports whether a registry repo path is a BuildKit cache
// repo. The repo may include a host prefix (e.g.
// "10.0.0.1:8083/cache/global/node").
func isCacheRepo(repo string) bool {
	return strings.HasPrefix(repo, "cache/") || strings.Contains(repo, "/cache/")
}

// Handler returns the http.Handler for the registry endpoints.
// This wraps the underlying registry to intercept manifest PUTs and trigger conversion.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Intercept manifest PUT requests to store in blob store and trigger conversion
		if req.Method == http.MethodPut {
			matches := manifestPutPattern.FindStringSubmatch(req.URL.Path)
			if matches != nil {
				pathRepo := matches[1]
				reference := matches[2]

				body, err := io.ReadAll(req.Body)
				req.Body.Close()
				if err != nil {
					http.Error(w, "failed to read body", http.StatusInternalServerError)
					return
				}

				digest := computeDigest(body)

				// Verify digest if reference is a digest
				if strings.HasPrefix(reference, "sha256:") && reference != digest {
					http.Error(w, fmt.Sprintf("digest mismatch: expected %s, got %s", reference, digest), http.StatusBadRequest)
					return
				}

				if err := r.storeManifestBlob(digest, body); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to store manifest blob: %v\n", err)
				}

				req.Body = io.NopCloser(bytes.NewReader(body))
				wrapper := &responseWrapper{ResponseWriter: w}
				r.handler.ServeHTTP(wrapper, req)

				if wrapper.statusCode == http.StatusCreated {
					if isCacheRepo(pathRepo) {
						r.recordCacheTag(pathRepo, reference, digest)
					}
					// Use pathRepo (without registry host prefix) so pushed images
					// are stored under their short name. This ensures consistency:
					// `hypeman push myapp` stores as "docker.io/library/myapp:latest"
					// which matches what `hypeman run myapp` looks up.
					go r.triggerConversion(pathRepo, reference, digest)
				}
				return
			}
		}

		r.handler.ServeHTTP(w, req)
	})
}

// storeManifestBlob stores a manifest in the blob store by its digest.
func (r *Registry) storeManifestBlob(digest string, data []byte) error {
	digestHex := strings.TrimPrefix(digest, "sha256:")
	blobPath := r.paths.OCICacheBlob(digestHex)

	// Verify digest matches
	actualDigest := computeDigest(data)
	if actualDigest != digest {
		return fmt.Errorf("digest mismatch: expected %s, got %s", digest, actualDigest)
	}

	return os.WriteFile(blobPath, data, 0644)
}

// responseWrapper captures the status code from the response
type responseWrapper struct {
	http.ResponseWriter
	statusCode int
}

func (w *responseWrapper) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

// triggerConversion queues the image for conversion to ext4 disk format.
// Skips BuildKit cache images (cache/*) since they're not runnable containers.
func (r *Registry) triggerConversion(repo, reference, dockerDigest string) {
	// Skip BuildKit cache images - they use a custom mediatype that can't be
	// unpacked as a standard OCI image. BuildKit imports them directly from
	// the registry without needing local conversion.
	// Note: repo may include host prefix (e.g., "10.102.0.1:8083/cache/global/node")
	if isCacheRepo(repo) {
		return
	}

	imageRef := repo + ":" + reference
	if strings.HasPrefix(reference, "sha256:") {
		imageRef = repo + "@" + reference
	}

	ociDigest, err := r.addToOCILayout(dockerDigest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to add image to OCI layout for %s: %v\n", imageRef, err)
		return
	}

	_, err = r.imageManager.ImportLocalImage(context.Background(), repo, reference, ociDigest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to queue image conversion for %s: %v\n", imageRef, err)
	}
}

// addToOCILayout adds the image to the OCI layout, converting Docker v2 to OCI if needed.
func (r *Registry) addToOCILayout(inputDigest string) (string, error) {
	cacheDir := r.paths.SystemOCICache()
	path, err := layout.FromPath(cacheDir)
	if err != nil {
		path, err = layout.Write(cacheDir, empty.Index)
		if err != nil {
			return "", fmt.Errorf("create oci layout: %w", err)
		}
	}

	img, err := ocicache.ImageFromCache(r.paths, inputDigest)
	if err != nil {
		return "", fmt.Errorf("create image from blob store: %w", err)
	}

	digestHash, err := img.Digest()
	if err != nil {
		return "", fmt.Errorf("compute digest: %w", err)
	}
	digest := digestHash.String()
	digestHex := digestHash.Hex

	err = path.AppendImage(img, layout.WithAnnotations(map[string]string{
		"org.opencontainers.image.ref.name": digestHex,
	}))
	if err != nil {
		return "", fmt.Errorf("append image to layout: %w", err)
	}

	return digest, nil
}

// computeDigest calculates SHA256 hash of data
func computeDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
