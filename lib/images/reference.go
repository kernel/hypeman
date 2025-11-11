package images

import (
	"context"
	"strings"

	"github.com/distribution/reference"
)

// NormalizedRef is a validated and normalized OCI image reference.
// It can be either a tagged reference (e.g., "docker.io/library/alpine:latest")
// or a digest reference (e.g., "docker.io/library/alpine@sha256:abc123...").
type NormalizedRef struct {
	raw        string
	repository string
	tag        string // empty if digest ref
	digest     string // empty if tag ref
	isDigest   bool
}

// ParseNormalizedRef validates and normalizes a user-provided image reference.
// Examples:
//   - "alpine" -> "docker.io/library/alpine:latest"
//   - "alpine:3.18" -> "docker.io/library/alpine:3.18"
//   - "alpine@sha256:abc..." -> "docker.io/library/alpine@sha256:abc..."
func ParseNormalizedRef(s string) (*NormalizedRef, error) {
	named, err := reference.ParseNormalizedNamed(s)
	if err != nil {
		return nil, err
	}

	ref := &NormalizedRef{}

	// Extract repository (always present)
	ref.repository = reference.Domain(named) + "/" + reference.Path(named)

	// If it's canonical (has digest), extract digest
	if canonical, ok := named.(reference.Canonical); ok {
		ref.isDigest = true
		ref.digest = canonical.Digest().String()
		ref.raw = canonical.String()
		return ref, nil
	}

	// Otherwise it's a tagged reference - ensure tag (add :latest if missing)
	tagged := reference.TagNameOnly(named)
	if t, ok := tagged.(reference.Tagged); ok {
		ref.tag = t.Tag()
	}
	ref.raw = tagged.String()

	return ref, nil
}

// String returns the full normalized reference.
func (r *NormalizedRef) String() string {
	return r.raw
}

// IsDigest returns true if this reference contains a digest (@sha256:...).
func (r *NormalizedRef) IsDigest() bool {
	return r.isDigest
}

// Digest returns the digest if present (e.g., "sha256:abc123...").
// Returns empty string if this is a tagged reference.
func (r *NormalizedRef) Digest() string {
	return r.digest
}

// Repository returns the repository path without tag or digest.
// Example: "docker.io/library/alpine"
func (r *NormalizedRef) Repository() string {
	return r.repository
}

// Tag returns the tag if this is a tagged reference (e.g., "latest").
// Returns empty string if this is a digest reference.
func (r *NormalizedRef) Tag() string {
	return r.tag
}

// DigestHex returns just the hex portion of the digest (without "sha256:" prefix).
// Returns empty string if this is a tagged reference.
func (r *NormalizedRef) DigestHex() string {
	if r.digest == "" {
		return ""
	}

	// Strip "sha256:" prefix
	parts := strings.SplitN(r.digest, ":", 2)
	if len(parts) != 2 {
		return "" // Invalid format
	}

	return parts[1]
}

// ResolvedRef is a NormalizedRef that has been resolved to include the actual
// manifest digest from the registry. The digest is always present.
type ResolvedRef struct {
	normalized *NormalizedRef
	digest     string // Always populated (e.g., "sha256:abc123...")
}

// NewResolvedRef creates a ResolvedRef from a NormalizedRef and digest.
func NewResolvedRef(normalized *NormalizedRef, digest string) *ResolvedRef {
	return &ResolvedRef{
		normalized: normalized,
		digest:     digest,
	}
}

// String returns the full normalized reference (the original user input format).
func (r *ResolvedRef) String() string {
	return r.normalized.String()
}

// Repository returns the repository path without tag or digest.
// Example: "docker.io/library/alpine"
func (r *ResolvedRef) Repository() string {
	return r.normalized.Repository()
}

// Tag returns the tag if this was originally a tagged reference (e.g., "latest").
// Returns empty string if this was originally a digest reference.
func (r *ResolvedRef) Tag() string {
	return r.normalized.Tag()
}

// Digest returns the resolved manifest digest (e.g., "sha256:abc123...").
// This is always populated after resolution.
func (r *ResolvedRef) Digest() string {
	return r.digest
}

// DigestHex returns just the hex portion of the digest (without "sha256:" prefix).
func (r *ResolvedRef) DigestHex() string {
	// Strip "sha256:" prefix
	parts := strings.SplitN(r.digest, ":", 2)
	if len(parts) != 2 {
		return "" // Invalid format
	}
	return parts[1]
}

// Resolve inspects the manifest to get the digest and returns a ResolvedRef.
// This requires an ociClient interface for manifest inspection.
type ManifestInspector interface {
	inspectManifest(ctx context.Context, imageRef string) (string, error)
}

// Resolve returns a ResolvedRef by inspecting the manifest to get the authoritative digest.
func (r *NormalizedRef) Resolve(ctx context.Context, inspector ManifestInspector) (*ResolvedRef, error) {
	digest, err := inspector.inspectManifest(ctx, r.String())
	if err != nil {
		return nil, err
	}
	return NewResolvedRef(r, digest), nil
}
