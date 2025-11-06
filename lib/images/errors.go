package images

import "errors"

var (
	ErrNotFound      = errors.New("image not found")
	ErrAlreadyExists = errors.New("image already exists")
)

