package images

import (
	"errors"
	"fmt"
	"strings"
)

// RateLimitMessage is the actionable response returned to API clients when a
// registry rejects an image request because its rate limit was exceeded.
const RateLimitMessage = "registry rate limit exceeded; retry later or authenticate to the registry"

var (
	ErrNotFound        = errors.New("image not found")
	ErrInvalidName     = errors.New("invalid image name")
	ErrInvalidPlatform = errors.New("invalid platform")
	// ErrPlatformNotAvailable means the requested platform is well-formed but the
	// image's manifest index does not publish a matching variant. Unlike
	// ErrInvalidPlatform (bad user syntax), the platform itself is valid; the
	// image simply does not provide it.
	ErrPlatformNotAvailable = errors.New("platform not available for image")
	// ErrRateLimited means the registry rejected the request with a rate-limit
	// response (e.g. Docker Hub's unauthenticated pull limit). Transient and
	// caller-actionable (retry later or authenticate), not a server fault.
	ErrRateLimited = errors.New("registry rate limit exceeded")
)

// wrapRegistryError classifies a raw registry/go-containerregistry error into a
// typed hypeman error so the API layer can map it to an appropriate 4xx/429
// status instead of a blanket 500. Errors that don't match a known client- or
// registry-condition are returned unchanged (and surface as 500).
func wrapRegistryError(err error) error {
	if err == nil {
		return nil
	}
	// Match case-insensitively: registry codes (TOOMANYREQUESTS, NAME_UNKNOWN, ...)
	// lowercase cleanly, so a single lowercased haystack covers both the codes and
	// the human-readable messages.
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "toomanyrequests") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "rate limit"):
		return fmt.Errorf("%w: %v", ErrRateLimited, err)
	case strings.Contains(lower, "no child with platform"):
		return fmt.Errorf("%w: %v", ErrPlatformNotAvailable, err)
	case strings.Contains(lower, "name_unknown") ||
		strings.Contains(lower, "manifest_unknown") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "404") ||
		strings.Contains(lower, "not found"):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	default:
		return err
	}
}
