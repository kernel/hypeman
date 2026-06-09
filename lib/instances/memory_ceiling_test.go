package instances

import (
	"errors"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
)

func TestValidateMemoryCeiling(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)

	// Zero means no ceiling: always valid, byte-identical to no-ceiling behavior,
	// on any hypervisor.
	if err := validateMemoryCeiling(0, gib, hypervisor.TypeCloudHypervisor); err != nil {
		t.Fatalf("ceiling 0 should be valid, got %v", err)
	}

	// A ceiling at or below the baseline is meaningless and rejected.
	for _, ceiling := range []int64{gib / 2, gib} {
		err := validateMemoryCeiling(ceiling, gib, hypervisor.TypeVZ)
		if err == nil {
			t.Fatalf("ceiling %d <= size %d should be rejected", ceiling, gib)
		}
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("expected ErrInvalidRequest for ceiling %d, got %v", ceiling, err)
		}
	}

	// A ceiling above the baseline is accepted on vz.
	if err := validateMemoryCeiling(4*gib, gib, hypervisor.TypeVZ); err != nil {
		t.Fatalf("ceiling above size should be valid on vz, got %v", err)
	}

	// A ceiling on a non-vz backend is rejected: those backends ignore it and
	// would mis-account the guest-memory controller.
	err := validateMemoryCeiling(4*gib, gib, hypervisor.TypeCloudHypervisor)
	if err == nil {
		t.Fatalf("ceiling on cloud-hypervisor should be rejected")
	}
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest for non-vz ceiling, got %v", err)
	}
}
