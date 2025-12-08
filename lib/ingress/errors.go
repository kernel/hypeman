package ingress

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Common errors returned by the ingress package.
var (
	// ErrNotFound is returned when an ingress is not found.
	ErrNotFound = errors.New("ingress not found")

	// ErrAlreadyExists is returned when trying to create an ingress that already exists.
	ErrAlreadyExists = errors.New("ingress already exists")

	// ErrInvalidRequest is returned when the request is invalid.
	ErrInvalidRequest = errors.New("invalid request")

	// ErrInstanceNotFound is returned when the target instance is not found.
	ErrInstanceNotFound = errors.New("target instance not found")

	// ErrInstanceNoNetwork is returned when the target instance has no network.
	ErrInstanceNoNetwork = errors.New("target instance has no network configured")

	// ErrHostnameInUse is returned when a hostname is already in use by another ingress.
	ErrHostnameInUse = errors.New("hostname already in use by another ingress")

	// ErrConfigValidationFailed is returned when Caddy config validation fails.
	// This indicates the config was rejected by Caddy's admin API.
	ErrConfigValidationFailed = errors.New("config validation failed")

	// ErrPortInUse is returned when the requested port is already in use by another process.
	ErrPortInUse = errors.New("port already in use")
)

// portInUseRegex matches Caddy's "address already in use" error messages
var portInUseRegex = regexp.MustCompile(`listen tcp [^:]+:(\d+): bind: address already in use`)

// ParseCaddyError parses a Caddy error response and returns a more specific error if possible.
func ParseCaddyError(caddyError string) error {
	// Check for "address already in use" errors
	if strings.Contains(caddyError, "address already in use") {
		if matches := portInUseRegex.FindStringSubmatch(caddyError); len(matches) > 1 {
			return fmt.Errorf("%w: port %s is already bound by another process", ErrPortInUse, matches[1])
		}
		return fmt.Errorf("%w: address is already bound by another process", ErrPortInUse)
	}

	return nil
}
