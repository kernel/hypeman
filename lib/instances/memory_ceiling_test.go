package instances

import (
	"errors"
	"testing"
)

func TestValidateMemoryCeiling(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)

	// Zero means no ceiling: always valid, byte-identical to no-ceiling behavior.
	if err := validateMemoryCeiling(0, gib); err != nil {
		t.Fatalf("ceiling 0 should be valid, got %v", err)
	}

	// A ceiling at or below the baseline is meaningless and rejected.
	for _, ceiling := range []int64{gib / 2, gib} {
		err := validateMemoryCeiling(ceiling, gib)
		if err == nil {
			t.Fatalf("ceiling %d <= size %d should be rejected", ceiling, gib)
		}
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("expected ErrInvalidRequest for ceiling %d, got %v", ceiling, err)
		}
	}

	// A ceiling above the baseline is accepted.
	if err := validateMemoryCeiling(4*gib, gib); err != nil {
		t.Fatalf("ceiling above size should be valid, got %v", err)
	}
}
