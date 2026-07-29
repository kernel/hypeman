package instances

import (
	"fmt"

	"github.com/kernel/hypeman/lib/hypervisor"
)

func resolveCreateHypervisor(req CreateInstanceRequest, defaultHypervisor hypervisor.Type) (hypervisor.Type, error) {
	hvType := req.Hypervisor
	if hvType == "" {
		hvType = defaultHypervisor
	}

	profile := ""
	if req.GPU != nil {
		profile = req.GPU.Profile
	}
	if err := validateVGPUHypervisor(profile, hvType); err != nil {
		return "", err
	}
	return hvType, nil
}

func validateVGPUHypervisor(profile string, hvType hypervisor.Type) error {
	if profile != "" && hvType != hypervisor.TypeQEMU {
		return fmt.Errorf("%w: vGPU requires qemu, got %s", ErrInvalidRequest, hvType)
	}
	return nil
}
