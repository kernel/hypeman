package system

import "errors"

var (
	// ErrUnsupportedVersion is returned when a version is not supported
	ErrUnsupportedVersion = errors.New("unsupported version")

	// ErrDownloadFailed is returned when downloading system files fails
	ErrDownloadFailed = errors.New("download failed")

	// ErrBuildFailed is returned when building initrd fails
	ErrBuildFailed = errors.New("build failed")
)

