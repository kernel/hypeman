package instances

import (
	"fmt"
	"time"
)

func validateExpiresAt(expiresAt *time.Time, now time.Time) error {
	if expiresAt != nil && !expiresAt.After(now) {
		return fmt.Errorf("%w: must be in the future", ErrInvalidExpiresAt)
	}
	return nil
}
