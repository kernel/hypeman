package instances

import (
	"fmt"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// validateMemoryCeiling enforces the boot-ceiling contract: zero means no
// ceiling (today's behavior), a non-zero ceiling must exceed the baseline so
// there is room for the guest to grow above its normal running size, and a
// ceiling is vz-only because it is realized by booting the VM larger and
// ballooning down — backends with real hotplug ignore it, so accepting one for
// them would mis-account the guest-memory controller. The host-RAM upper bound
// is rejected later by the shim, the only place the framework's host-derived
// maximum is available. Pure function so it can be unit-tested without booting a
// VM.
func validateMemoryCeiling(ceilingBytes, sizeBytes int64, hvType hypervisor.Type) error {
	if ceilingBytes == 0 {
		return nil
	}
	if hvType != hypervisor.TypeVZ {
		return fmt.Errorf("%w: memory ceiling is only supported on the vz hypervisor, not %s", ErrInvalidRequest, hvType)
	}
	if ceilingBytes <= sizeBytes {
		return fmt.Errorf("%w: memory ceiling %d must be greater than size %d", ErrInvalidRequest, ceilingBytes, sizeBytes)
	}
	return nil
}
