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
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/paths"
)

// Registry provides an OCI Distribution Spec compliant registry that stores pushed images
// in hypeman's OCI cache and triggers conversion to ext4 disk format.
type Registry struct {
	paths        *paths.Paths
	imageManager images.Manager
	blobStore    *BlobStore
	handler      http.Handler
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
	}

	return r, nil
}

// Handler returns the http.Handler for the registry endpoints.
// This wraps the underlying registry to intercept manifest PUTs and trigger conversion.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Intercept manifest PUT requests to store in blob store and trigger conversion
		if req.Method == http.MethodPut {
			matches := manifestPutPattern.FindStringSubmatch(req.URL.Path)
			if matches != nil {
				repo := matches[1]
				reference := matches[2]

				// Read the manifest body so we can store it in our blob store
				// go-containerregistry stores manifests in-memory, but we need them on disk
				body, err := io.ReadAll(req.Body)
				req.Body.Close()
				if err != nil {
					http.Error(w, "failed to read body", http.StatusInternalServerError)
					return
				}

				// Store manifest in blob store if reference is a digest
				if strings.HasPrefix(reference, "sha256:") {
					if err := r.storeManifestBlob(reference, body); err != nil {
						fmt.Fprintf(os.Stderr, "Warning: failed to store manifest blob: %v\n", err)
					}
				}

				// Reconstruct request body for the underlying handler
				req.Body = io.NopCloser(bytes.NewReader(body))

				// Wrap the response writer to capture the status code
				wrapper := &responseWrapper{ResponseWriter: w}

				// Let the underlying registry handle the request
				r.handler.ServeHTTP(wrapper, req)

				// If manifest was successfully stored, trigger conversion
				if wrapper.statusCode == http.StatusCreated {
					go r.triggerConversion(repo, reference)
				}
				return
			}
		}

		// Pass through all other requests
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
func (r *Registry) triggerConversion(repo, reference string) {
	// Build the full image reference for logging
	imageRef := repo + ":" + reference
	if strings.HasPrefix(reference, "sha256:") {
		imageRef = repo + "@" + reference
	}

	// Update OCI layout index so the existing image pipeline can find it
	if err := r.updateOCILayoutIndex(repo, reference); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to update OCI layout index for %s: %v\n", imageRef, err)
	}

	// For pushed images, we need the digest. If reference is already a digest, use it.
	// Otherwise, we need to look it up (but for now, we only support digest references for conversion)
	var digest string
	if strings.HasPrefix(reference, "sha256:") {
		digest = reference
	} else {
		// For tag references, skip conversion trigger - the client should also push by digest
		fmt.Fprintf(os.Stderr, "Warning: skipping conversion for tag reference %s (push by digest to trigger conversion)\n", imageRef)
		return
	}

	// Queue image conversion via image manager using ImportLocalImage
	_, err := r.imageManager.ImportLocalImage(context.Background(), repo, reference, digest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to queue image conversion for %s: %v\n", imageRef, err)
	}
}

// ociIndex represents the OCI image index structure
type ociIndex struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	Manifests     []ociManifestDesc `json:"manifests"`
}

type ociManifestDesc struct {
	MediaType   string            `json:"mediaType"`
	Size        int64             `json:"size"`
	Digest      string            `json:"digest"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// updateOCILayoutIndex updates the OCI layout index.json with the new manifest.
func (r *Registry) updateOCILayoutIndex(repo, reference string) error {
	indexPath := r.paths.OCICacheIndex()
	layoutPath := r.paths.OCICacheLayout()

	// Ensure oci-layout file exists
	if _, err := os.Stat(layoutPath); os.IsNotExist(err) {
		layout := `{"imageLayoutVersion": "1.0.0"}`
		if err := os.WriteFile(layoutPath, []byte(layout), 0644); err != nil {
			return fmt.Errorf("write oci-layout: %w", err)
		}
	}

	// Determine digest - if reference is a digest, use it directly
	var digest string
	var size int64
	var mediaType string
	if strings.HasPrefix(reference, "sha256:") {
		digest = reference
		digestHex := strings.TrimPrefix(digest, "sha256:")
		manifestPath := r.paths.OCICacheBlob(digestHex)
		if data, err := os.ReadFile(manifestPath); err == nil {
			size = int64(len(data))
			// Extract mediaType from manifest
			var manifest struct {
				MediaType string `json:"mediaType"`
			}
			if json.Unmarshal(data, &manifest) == nil && manifest.MediaType != "" {
				mediaType = manifest.MediaType
			}
		}
		if mediaType == "" {
			mediaType = "application/vnd.oci.image.manifest.v1+json"
		}
	} else {
		// For tags, skip - the digest reference push will handle it
		return nil
	}

	// Read existing index or create new one
	var index ociIndex
	if data, err := os.ReadFile(indexPath); err == nil {
		if err := json.Unmarshal(data, &index); err != nil {
			return fmt.Errorf("parse index.json: %w", err)
		}
	} else {
		index = ociIndex{
			SchemaVersion: 2,
			MediaType:     "application/vnd.oci.image.index.v1+json",
			Manifests:     []ociManifestDesc{},
		}
	}

	// Use digest hex as the layout tag
	digestHex := strings.TrimPrefix(digest, "sha256:")

	// Check if this manifest already exists in the index
	found := false
	for i, m := range index.Manifests {
		if m.Digest == digest {
			if m.Annotations == nil {
				index.Manifests[i].Annotations = make(map[string]string)
			}
			index.Manifests[i].Annotations["org.opencontainers.image.ref.name"] = digestHex
			found = true
			break
		}
	}

	if !found {
		desc := ociManifestDesc{
			MediaType: mediaType,
			Size:      size,
			Digest:    digest,
			Annotations: map[string]string{
				"org.opencontainers.image.ref.name": digestHex,
			},
		}
		index.Manifests = append(index.Manifests, desc)
	}

	// Write updated index
	indexData, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal index.json: %w", err)
	}

	if err := os.WriteFile(indexPath, indexData, 0644); err != nil {
		return fmt.Errorf("write index.json: %w", err)
	}

	return nil
}

// computeDigest calculates SHA256 hash of data
func computeDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
