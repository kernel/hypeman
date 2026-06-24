package api

import "fmt"

// validateStopTimeout normalizes an optional stop_timeout (seconds) from a
// request body, rejecting non-positive values. A nil input or result means the
// caller falls back to the instance's configured value or the server default.
func validateStopTimeout(v *int) (*int, error) {
	if v != nil && *v < 1 {
		return nil, fmt.Errorf("stop_timeout must be at least 1 second")
	}
	return v, nil
}
