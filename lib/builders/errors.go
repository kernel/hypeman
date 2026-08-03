package builders

import "errors"

var (
	// ErrNotFound is returned when a builder is not found
	ErrNotFound = errors.New("builder not found")

	// ErrAlreadyExists is returned when a builder with the same ID already exists
	ErrAlreadyExists = errors.New("builder already exists")

	// ErrInUse is returned when a builder is acquired by a build, has its
	// disk attached, or is mid-prune/delete
	ErrInUse = errors.New("builder is in use")

	// ErrInvalidID is returned when a caller-supplied builder ID is invalid
	ErrInvalidID = errors.New("invalid builder id")

	// ErrQuotaExceeded is returned when a create would exceed the configured
	// builder count limit
	ErrQuotaExceeded = errors.New("builder quota exceeded")
)
