package qemu

import (
	"fmt"
	"runtime"
)

// MachineType identifies a QEMU machine/board type persisted in QEMU's private
// restore contract. It is not part of the hypervisor-agnostic VM config.
type MachineType string

const (
	// MachineTypeQ35 is QEMU's standard x86 board.
	MachineTypeQ35 MachineType = "q35"
	// MachineTypeVirt is QEMU's standard ARM board.
	MachineTypeVirt MachineType = "virt"
	// MachineTypeMicroVM is QEMU's minimal x86-only board.
	MachineTypeMicroVM MachineType = "microvm"
)

func standardMachineType() MachineType {
	return standardMachineTypeForArch(runtime.GOARCH)
}

func standardMachineTypeForArch(goarch string) MachineType {
	switch goarch {
	case "amd64":
		return MachineTypeQ35
	case "arm64":
		return MachineTypeVirt
	default:
		// The API startup check rejects unsupported architectures. Keep this
		// panic for direct library callers so they cannot silently select a board.
		panic(fmt.Sprintf("unsupported QEMU architecture %s", goarch))
	}
}

func microVMMachineType() (MachineType, error) {
	return resolveMachineTypeForPlatform(MachineTypeMicroVM, runtime.GOOS, runtime.GOARCH)
}

func resolveMachineTypeForPlatform(requested MachineType, goos, goarch string) (MachineType, error) {
	switch goarch {
	case "amd64":
		if requested == MachineTypeQ35 {
			return requested, nil
		}
		if requested == MachineTypeMicroVM && goos == "linux" {
			return requested, nil
		}
	case "arm64":
		if requested == MachineTypeVirt {
			return MachineTypeVirt, nil
		}
	default:
		return "", fmt.Errorf("unsupported platform %s/%s", goos, goarch)
	}

	return "", fmt.Errorf("machine type %q is not supported on %s/%s", requested, goos, goarch)
}
