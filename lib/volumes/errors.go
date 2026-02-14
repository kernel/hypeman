package volumes

import "errors"

var (
	ErrNotFound      = errors.New("volume not found")
	ErrInUse         = errors.New("volume is in use")
	ErrAlreadyExists = errors.New("volume already exists")
	ErrAmbiguousName = errors.New("multiple volumes with the same name")
)

