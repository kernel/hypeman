package snapshottransfer

import "errors"

var (
	ErrTransferNotFound = errors.New("snapshot transfer not found")
	ErrSessionNotFound  = errors.New("snapshot import session not found")
	ErrConflict         = errors.New("snapshot transfer conflict")
	ErrInvalidRequest   = errors.New("invalid snapshot transfer request")
	ErrNotSupported     = errors.New("snapshot transfer not supported")
)
