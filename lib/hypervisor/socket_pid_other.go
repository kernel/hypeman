//go:build !linux

package hypervisor

import "fmt"

// ResolveProcessPID is only implemented on Linux, where the project relies on
// /proc socket metadata for runtime PID discovery.
func ResolveProcessPID(socketPath string) (int, error) {
	return 0, fmt.Errorf("resolve process pid for socket %s: not supported on this platform", socketPath)
}
