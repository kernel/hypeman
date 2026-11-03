// Package registry implements an OCI Distribution Spec registry that accepts pushed images
// and triggers conversion to hypeman's disk format.
package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
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
	uploads      *chunkedUploads
	handler      http.Handler

	// pushedTags tracks pushed (repo, reference) tags to their manifest
	// digest. The underlying go-containerregistry registry keeps tags in
	// memory and the Docker manifest blob is not reachable from the OCI
	// cache index.json (the layout stores the OCI-converted manifest under a
	// different digest), so without this map the OCI cache GC would sweep
	// the manifest blobs the registry is still serving on tag/digest pulls.
	// Cleared with the process; tags do not survive restart.
	pushedTagsMu sync.RWMutex
	// manifestReposMu serializes recording which repositories a manifest was pushed to.
	manifestReposMu sync.Mutex
	pushedTags      map[string]string
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

	uploads, err := newChunkedUploads(filepath.Join(p.SystemOCICache(), "uploads"), blobStore)
	if err != nil {
		return nil, fmt.Errorf("create upload directory: %w", err)
	}

	// Create registry with custom blob handler
	regHandler := registry.New(
		registry.WithBlobHandler(blobStore),
	)

	r := &Registry{
		paths:        p,
		imageManager: imgManager,
		blobStore:    blobStore,
		uploads:      uploads,
		handler:      regHandler,
		pushedTags:   make(map[string]string),
	}

	return r, nil
}

// LiveCacheManifestDigests returns the manifest digests of every tag the
// registry has accepted since startup, including BuildKit cache exports and
// regular image pushes. Used by the OCI cache GC as additional roots: the
// in-memory registry never adds these to index.json (and the Docker manifest
// blob a push stores is only reachable this way), so without these roots the
// GC would sweep blobs that are still being served to clients.
func (r *Registry) LiveCacheManifestDigests() []string {
	r.pushedTagsMu.RLock()
	defer r.pushedTagsMu.RUnlock()
	if len(r.pushedTags) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(r.pushedTags))
	out := make([]string, 0, len(r.pushedTags))
	for _, digest := range r.pushedTags {
		if _, ok := seen[digest]; ok {
			continue
		}
		seen[digest] = struct{}{}
		out = append(out, digest)
	}
	return out
}

// recordPushedTag stores the (repo, reference) -> digest mapping for a
// manifest push. Replaces any prior digest for the same tag.
func (r *Registry) recordPushedTag(repo, reference, digest string) {
	r.pushedTagsMu.Lock()
	defer r.pushedTagsMu.Unlock()
	r.pushedTags[repo+":"+reference] = digest
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
		if r.uploads.serve(w, req) || r.serveManifestByDigest(w, req) {
			return
		}
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
					if err := r.recordManifestRepo(pathRepo, digest); err != nil {
						fmt.Fprintf(os.Stderr, "Warning: failed to record manifest repository: %v\n", err)
					}
					// Root every pushed manifest: the Docker manifest blob is
					// served by the registry but not reachable from the OCI
					// layout (which stores the converted manifest), so GC
					// would otherwise sweep it and break tag/digest pulls.
					r.recordPushedTag(pathRepo, reference, digest)
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

// serveManifestByDigest answers a manifest read by digest, from the upstream
// registry or, when it does not know the manifest, from the blob store, where every
// pushed manifest is kept, if it was pushed to the requested repository. The
// upstream registry holds manifests in memory, so a manifest pushed before a
// restart is otherwise unknown by digest. Other requests are not handled.
func (r *Registry) serveManifestByDigest(w http.ResponseWriter, req *http.Request) bool {
	matches := manifestPutPattern.FindStringSubmatch(req.URL.Path)
	if matches == nil || (req.Method != http.MethodGet && req.Method != http.MethodHead) {
		return false
	}
	h, err := v1.NewHash(matches[2])
	if err != nil {
		return false
	}
	// Manifests are small, so the upstream answer is buffered to fall back on a 404.
	upstream := &bufferedResponse{header: make(http.Header), status: http.StatusOK}
	r.handler.ServeHTTP(upstream, req)
	if upstream.status != http.StatusNotFound || !r.manifestPushedTo(matches[1], h) || !r.serveStoredManifest(w, req, h) {
		for key, values := range upstream.header {
			w.Header()[key] = values
		}
		w.WriteHeader(upstream.status)
		w.Write(upstream.body.Bytes())
	}
	return true
}

// bufferedResponse records a response for serveManifestByDigest.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header            { return b.header }
func (b *bufferedResponse) WriteHeader(status int)         { b.status = status }
func (b *bufferedResponse) Write(data []byte) (int, error) { return b.body.Write(data) }

// serveStoredManifest writes the manifest h from the blob store and reports
// whether it had one.
func (r *Registry) serveStoredManifest(w http.ResponseWriter, req *http.Request, h v1.Hash) bool {
	path := r.blobStore.blobPath(h.String())
	if info, err := os.Stat(path); err != nil || info.Size() > maxStoredManifestBytes {
		return false
	}
	data, err := os.ReadFile(path)
	var manifest struct {
		MediaType string `json:"mediaType"`
	}
	if err != nil || json.Unmarshal(data, &manifest) != nil || manifest.MediaType == "" || computeDigest(data) != h.String() {
		return false
	}
	w.Header().Set("Content-Type", manifest.MediaType)
	w.Header().Set("Docker-Content-Digest", h.String())
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if req.Method == http.MethodGet {
		w.Write(data)
	}
	return true
}

// manifestReposPath lists, one per line, the repositories a manifest was pushed to.
func (r *Registry) manifestReposPath(h v1.Hash) string {
	return filepath.Join(r.paths.SystemOCICache(), "manifest-repos", h.Hex)
}

// recordManifestRepo records that the manifest digest was pushed to repo.
func (r *Registry) recordManifestRepo(repo, digest string) error {
	h, err := v1.NewHash(digest)
	if err != nil {
		return err
	}
	r.manifestReposMu.Lock()
	defer r.manifestReposMu.Unlock()
	if r.manifestPushedTo(repo, h) {
		return nil
	}
	path := r.manifestReposPath(h)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	_, err = file.WriteString(repo + "\n")
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// manifestPushedTo reports whether the manifest h was pushed to repo.
func (r *Registry) manifestPushedTo(repo string, h v1.Hash) bool {
	data, err := os.ReadFile(r.manifestReposPath(h))
	return err == nil && slices.Contains(strings.Split(string(data), "\n"), repo)
}

// maxStoredManifestBytes bounds a stored blob served as a manifest, so a layer
// requested as one is not read into memory.
const maxStoredManifestBytes = 4 << 20

// storeManifestBlob stores a manifest in the blob store by its digest.
func (r *Registry) storeManifestBlob(digest string, data []byte) error {
	digestHex := strings.TrimPrefix(digest, "sha256:")

	// Verify digest matches
	actualDigest := computeDigest(data)
	if actualDigest != digest {
		return fmt.Errorf("digest mismatch: expected %s, got %s", digest, actualDigest)
	}

	return ocicache.WriteBlob(r.paths.SystemOCICache(), v1.Hash{Algorithm: "sha256", Hex: digestHex}, data)
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

	err = ocicache.AppendImage(cacheDir, img, digestHex)
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
