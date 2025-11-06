package instances

import "errors"

var (
	ErrNotFound     = errors.New("instance not found")
	ErrInvalidState = errors.New("invalid instance state for this operation")
)

