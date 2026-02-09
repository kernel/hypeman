package api

import (
	"encoding/json"
	"net/http"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/logger"
)

// MirrorBaseImageRequest is the request body for POST /admin/mirror-base-image
type MirrorBaseImageRequest struct {
	// SourceImage is the full image reference to pull from (e.g., "docker.io/onkernel/nodejs22-base:0.1.1")
	SourceImage string `json:"source_image"`
}

// MirrorBaseImageResponse is the response body for POST /admin/mirror-base-image
type MirrorBaseImageResponse struct {
	// SourceImage is the original image reference
	SourceImage string `json:"source_image"`
	// LocalRef is the local registry reference (e.g., "onkernel/nodejs22-base:0.1.1")
	LocalRef string `json:"local_ref"`
	// Digest is the image digest
	Digest string `json:"digest"`
}

// MirrorBaseImageHandler returns an HTTP handler for mirroring base images.
// This is an admin endpoint that pulls images from external registries and
// pushes them to the local registry with the same normalized name.
//
// Example usage:
//
//	POST /admin/mirror-base-image
//	{"source_image": "docker.io/onkernel/nodejs22-base:0.1.1"}
//
// Response:
//
//	{"source_image": "...", "local_ref": "onkernel/nodejs22-base:0.1.1", "digest": "sha256:..."}
func MirrorBaseImageHandler(registryURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log := logger.FromContext(r.Context())

		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse request body
		var req MirrorBaseImageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.ErrorContext(r.Context(), "failed to decode request body", "error", err)
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		if req.SourceImage == "" {
			http.Error(w, "source_image is required", http.StatusBadRequest)
			return
		}

		log.InfoContext(r.Context(), "mirroring base image", "source", req.SourceImage)

		// Mirror the image
		result, err := images.MirrorBaseImage(r.Context(), registryURL, images.MirrorRequest{
			SourceImage: req.SourceImage,
		}, nil) // No auth config for local insecure registry
		if err != nil {
			log.ErrorContext(r.Context(), "failed to mirror base image", "error", err, "source", req.SourceImage)
			http.Error(w, "failed to mirror image: "+err.Error(), http.StatusInternalServerError)
			return
		}

		log.InfoContext(r.Context(), "base image mirrored successfully",
			"source", result.SourceImage,
			"local_ref", result.LocalRef,
			"digest", result.Digest)

		// Return response
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(MirrorBaseImageResponse{
			SourceImage: result.SourceImage,
			LocalRef:    result.LocalRef,
			Digest:      result.Digest,
		})
	}
}
