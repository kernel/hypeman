package images

import "errors"

var (
	ErrNotFound         = errors.New("image not found")
	ErrAlreadyExists    = errors.New("image already exists")
	ErrInvalidImage     = errors.New("invalid image reference")
	ErrConversionFailed = errors.New("failed to convert image")
)

