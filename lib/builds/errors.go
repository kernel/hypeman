package builds

import "errors"

var (
	// ErrNotFound is returned when a build is not found
	ErrNotFound = errors.New("build not found")

	// ErrAlreadyExists is returned when a build with the same ID already exists
	ErrAlreadyExists = errors.New("build already exists")

	// ErrInvalidRuntime is returned when an unsupported runtime is specified
	ErrInvalidRuntime = errors.New("invalid runtime")

	// ErrBuildFailed is returned when a build fails
	ErrBuildFailed = errors.New("build failed")

	// ErrBuildTimeout is returned when a build exceeds its timeout
	ErrBuildTimeout = errors.New("build timeout")

	// ErrBuildCancelled is returned when a build is cancelled
	ErrBuildCancelled = errors.New("build cancelled")

	// ErrInvalidSource is returned when the source tarball is invalid
	ErrInvalidSource = errors.New("invalid source")

	// ErrSourceHashMismatch is returned when the source hash doesn't match
	ErrSourceHashMismatch = errors.New("source hash mismatch")

	// ErrBuilderNotReady is returned when the builder image is not available
	ErrBuilderNotReady = errors.New("builder image not ready")

	// ErrBuildInProgress is returned when trying to cancel a build that's already complete
	ErrBuildInProgress = errors.New("build in progress")

	// ErrResourcesExhausted is returned when a build cannot start due to resource limits
	ErrResourcesExhausted = errors.New("resources exhausted")
)

// IsSupportedRuntime returns true if the runtime is supported
func IsSupportedRuntime(runtime string) bool {
	switch runtime {
	case RuntimeNodeJS20, RuntimePython312:
		return true
	default:
		return false
	}
}

