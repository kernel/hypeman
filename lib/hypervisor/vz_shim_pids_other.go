//go:build !darwin

package hypervisor

// VZShimPIDs finds vz-shim processes, which only run on macOS hosts.
func VZShimPIDs(string) ([]int, error) { return nil, nil }
