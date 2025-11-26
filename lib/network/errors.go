package network

import "errors"

var (
	// ErrNotFound is returned when the default network is not found
	ErrNotFound = errors.New("network not found")

	// ErrNameExists is returned when an instance name already exists
	ErrNameExists = errors.New("instance name already exists")
)

