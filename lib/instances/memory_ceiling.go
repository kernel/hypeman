package instances

import "fmt"

// validateMemoryCeiling enforces the boot-ceiling contract: zero means no
// ceiling (today's behavior), and a non-zero ceiling must exceed the baseline
// so there is room for the guest to grow above its normal running size. The
// host-RAM upper bound is enforced later by the shim's vz configuration
// validation, which is the only place the framework's host-derived maximum is
// available. Pure function so it can be unit-tested without booting a VM.
func validateMemoryCeiling(ceilingBytes, sizeBytes int64) error {
	if ceilingBytes == 0 {
		return nil
	}
	if ceilingBytes <= sizeBytes {
		return fmt.Errorf("%w: memory ceiling %d must be greater than size %d", ErrInvalidRequest, ceilingBytes, sizeBytes)
	}
	return nil
}
