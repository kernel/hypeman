package images

import "errors"

var (
	ErrNotFound    = errors.New("image not found")
	ErrInvalidName = errors.New("invalid image name")
)
