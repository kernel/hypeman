package instances

import "errors"

var (
	// ErrNotFound is returned when an instance is not found
	ErrNotFound = errors.New("instance not found")

	// ErrInvalidState is returned when a state transition is not valid
	ErrInvalidState = errors.New("invalid state transition")

	// ErrInvalidRequest is returned when request validation fails
	ErrInvalidRequest = errors.New("invalid request")

	// ErrAlreadyExists is returned when creating an instance that already exists
	ErrAlreadyExists = errors.New("instance already exists")

	// ErrImageNotReady is returned when the image is not ready for use
	ErrImageNotReady = errors.New("image not ready")

	// ErrAmbiguousName is returned when multiple instances have the same name
	ErrAmbiguousName = errors.New("multiple instances with the same name")

	// ErrInsufficientResources is returned when resources (CPU, memory, network, GPU) are not available
	ErrInsufficientResources = errors.New("insufficient resources")

	// ErrNotSupported is returned when an operation is not supported for the instance hypervisor
	ErrNotSupported = errors.New("operation not supported")

	// ErrSnapshotNotFound is returned when a snapshot is not found.
	ErrSnapshotNotFound = errors.New("snapshot not found")
)
