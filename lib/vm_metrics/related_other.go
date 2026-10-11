//go:build !darwin

package vm_metrics

// relatedVMPIDs has no extra processes to sample outside macOS: the hypervisor PID is the guest.
func relatedVMPIDs(string) []int { return nil }
