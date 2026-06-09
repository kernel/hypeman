package instances

import (
	"errors"
	"runtime"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
)

func TestDeriveEnableRosetta(t *testing.T) {
	// A host-native image never enables Rosetta and never errors, regardless of
	// hypervisor or host.
	for _, hv := range []hypervisor.Type{hypervisor.TypeVZ, hypervisor.TypeCloudHypervisor, hypervisor.TypeFirecracker} {
		enable, err := deriveEnableRosetta(false, hv)
		if err != nil {
			t.Fatalf("host-native image with %s: unexpected error %v", hv, err)
		}
		if enable {
			t.Fatalf("host-native image with %s: Rosetta should be off", hv)
		}
	}

	canEmulate := runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"

	// Emulated image under vz: auto-on only on darwin/arm64, error otherwise.
	enable, err := deriveEnableRosetta(true, hypervisor.TypeVZ)
	if canEmulate {
		if err != nil {
			t.Fatalf("emulated image on vz/Apple-silicon: unexpected error %v", err)
		}
		if !enable {
			t.Fatal("emulated image on vz/Apple-silicon: Rosetta should auto-enable")
		}
	} else {
		if err == nil {
			t.Fatal("emulated image on non-Apple-silicon host should be rejected")
		}
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("expected ErrInvalidRequest, got %v", err)
		}
		if enable {
			t.Fatal("rejected create should not enable Rosetta")
		}
	}

	// Emulated image under a non-vz hypervisor is always rejected (Rosetta is
	// vz-only), even on Apple silicon.
	enable, err = deriveEnableRosetta(true, hypervisor.TypeCloudHypervisor)
	if err == nil {
		t.Fatal("emulated image on non-vz hypervisor should be rejected")
	}
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
	if enable {
		t.Fatal("rejected create should not enable Rosetta")
	}
}
