package images

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrNotFound    = errors.New("image not found")
	ErrInvalidName = errors.New("invalid image name")
)

// wrapRegistryError checks if the error is a registry 404 error and wraps it as ErrNotFound.
// go-containerregistry returns transport errors with specific codes for registry issues.
func wrapRegistryError(err error) error {
	if err == nil {
		return nil
	}
	errStr := err.Error()
	if strings.Contains(errStr, "NAME_UNKNOWN") ||
		strings.Contains(errStr, "MANIFEST_UNKNOWN") ||
		strings.Contains(errStr, "404") ||
		strings.Contains(errStr, "not found") {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return err
}
