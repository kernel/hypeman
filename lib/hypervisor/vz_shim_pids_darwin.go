//go:build darwin

package hypervisor

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// VZShimPIDs returns the vz-shim processes configured with socketPath as their
// control socket. The path is on the shim's command line before the shim
// listens, so this also finds a shim that is still restoring a VM.
func VZShimPIDs(socketPath string) ([]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-axww", "-o", "pid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	return vzShimPIDsFromPS(out, socketPath), nil
}
