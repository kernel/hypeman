package builds

import "errors"

var (
	// ErrNotFound is returned when a build is not found
	ErrNotFound = errors.New("build not found")

	// ErrAlreadyExists is returned when a build with the same ID already exists
	ErrAlreadyExists = errors.New("build already exists")

	// ErrDockerfileRequired is returned when no Dockerfile is provided
	ErrDockerfileRequired = errors.New("dockerfile required: provide dockerfile parameter or include Dockerfile in source tarball")

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

	// ErrBuildInProgress is returned when cancelling a queued build that was
	// already picked up and started running between the status check and the
	// queue removal.
	ErrBuildInProgress = errors.New("build in progress")

	// ErrInvalidSecretID is returned when a secret ID is empty or contains
	// path separators
	ErrInvalidSecretID = errors.New("invalid secret id")

	// ErrSecretNotFound is returned when a requested secret has no value
	ErrSecretNotFound = errors.New("secret not found")
)
