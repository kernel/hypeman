package volumes

import "errors"

var (
	ErrNotFound = errors.New("volume not found")
	ErrInUse    = errors.New("volume is in use")
)

