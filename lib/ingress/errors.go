package ingress

import "errors"

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
)
