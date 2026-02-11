package images

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// MirrorRequest contains the parameters for mirroring a base image
type MirrorRequest struct {
	// SourceImage is the full image reference to pull from (e.g., "docker.io/onkernel/nodejs22-base:0.1.1")
	SourceImage string
}

// MirrorResult contains the result of a mirror operation
type MirrorResult struct {
	// SourceImage is the original image reference
	SourceImage string `json:"source_image"`
	// LocalRef is the local registry reference (e.g., "onkernel/nodejs22-base:0.1.1")
	LocalRef string `json:"local_ref"`
	// Digest is the image digest
	Digest string `json:"digest"`
}

// MirrorBaseImage pulls an image from an external registry and pushes it to the
// local registry with the same normalized name. This enables Dockerfile FROM rewriting
// to use locally mirrored base images instead of pulling from Docker Hub.
//
// For example, mirroring "docker.io/onkernel/nodejs22-base:0.1.1" will create
// "onkernel/nodejs22-base:0.1.1" in the local registry.
func MirrorBaseImage(ctx context.Context, registryURL string, req MirrorRequest, authConfig *authn.AuthConfig) (*MirrorResult, error) {
	// Parse source reference
	srcRef, err := name.ParseReference(req.SourceImage)
	if err != nil {
		return nil, fmt.Errorf("parse source image reference: %w", err)
	}

	// Pull the image from source
	img, err := remote.Image(srcRef,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(vmPlatform()))
	if err != nil {
		return nil, fmt.Errorf("pull source image: %w", wrapRegistryError(err))
	}

	// Get the digest
	digest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("get image digest: %w", err)
	}

	// Build the local reference under bases/ namespace
	// Normalize the source to strip docker.io/ prefix for cleaner local refs
	localRef := normalizeToLocalRef(srcRef)

	// Strip any scheme from registry URL
	registryHost := stripScheme(registryURL)

	// Build full destination reference
	dstRefStr := fmt.Sprintf("%s/%s", registryHost, localRef)
	dstRef, err := name.ParseReference(dstRefStr)
	if err != nil {
		return nil, fmt.Errorf("parse destination reference: %w", err)
	}

	// Push to local registry
	// For insecure registries, we need to use the insecure transport
	opts := []remote.Option{
		remote.WithContext(ctx),
	}

	// If authConfig is provided, use it
	if authConfig != nil {
		opts = append(opts, remote.WithAuth(authn.FromConfig(*authConfig)))
	}

	if err := remote.Write(dstRef, img, opts...); err != nil {
		return nil, fmt.Errorf("push to local registry: %w", wrapRegistryError(err))
	}

	return &MirrorResult{
		SourceImage: req.SourceImage,
		LocalRef:    localRef,
		Digest:      digest.String(),
	}, nil
}

// normalizeToLocalRef converts a source image reference to a normalized local reference.
// It strips the docker.io/ prefix but preserves the library/ prefix for official images.
// The library/ prefix is kept because BuildKit's mirror protocol requests official images
// as library/<name> (e.g., /v2/library/node/manifests/...).
//
// Examples:
// - "docker.io/onkernel/nodejs22-base:0.1.1" -> "onkernel/nodejs22-base:0.1.1"
// - "docker.io/library/alpine:3.21" -> "library/alpine:3.21"
// - "node:20-alpine" -> "library/node:20-alpine" (go-containerregistry canonicalizes to library/)
// - "gcr.io/google-containers/pause:3.2" -> "gcr.io/google-containers/pause:3.2"
func normalizeToLocalRef(ref name.Reference) string {
	// Get the repository name (includes registry for non-Docker Hub images)
	repo := ref.Context().String()

	// Strip index.docker.io/ prefix (canonical form of docker.io)
	repo = strings.TrimPrefix(repo, "index.docker.io/")

	// Strip docker.io/ prefix
	repo = strings.TrimPrefix(repo, "docker.io/")

	// Keep library/ prefix — BuildKit mirror requests use it for official images

	// Build the tag or digest suffix
	var suffix string
	if tag, ok := ref.(name.Tag); ok {
		suffix = ":" + tag.TagStr()
	} else if dig, ok := ref.(name.Digest); ok {
		suffix = "@" + dig.DigestStr()
	}

	return repo + suffix
}

// stripScheme removes http:// or https:// prefix from a URL
func stripScheme(url string) string {
	url = strings.TrimPrefix(url, "https://")
	url = strings.TrimPrefix(url, "http://")
	return url
}
