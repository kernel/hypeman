package instances

import "errors"

var (
	// ErrNotFound is returned when an instance is not found
	ErrNotFound = errors.New("instance not found")

	// ErrInvalidState is returned when a state transition is not valid
	ErrInvalidState = errors.New("invalid state transition")

	// ErrAlreadyExists is returned when creating an instance that already exists
	ErrAlreadyExists = errors.New("instance already exists")

	// ErrImageNotReady is returned when the image is not ready for use
	ErrImageNotReady = errors.New("image not ready")

	// ErrAmbiguousName is returned when multiple instances have the same name
	ErrAmbiguousName = errors.New("multiple instances with the same name")

	// ErrResourcesExhausted is returned when an instance cannot be created due to resource limits
	ErrResourcesExhausted = errors.New("resources exhausted")
)
