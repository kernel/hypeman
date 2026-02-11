//go:build darwin

package vm_metrics

import "fmt"

// ReadProcStat is not available on macOS (/proc does not exist).
func ReadProcStat(pid int) (uint64, error) {
	return 0, fmt.Errorf("read proc stat: not supported on macOS")
}

// ReadProcStatm is not available on macOS (/proc does not exist).
func ReadProcStatm(pid int) (rssBytes, vmsBytes uint64, err error) {
	return 0, 0, fmt.Errorf("read proc statm: not supported on macOS")
}

// ReadTAPStats is not available on macOS (/sys does not exist).
func ReadTAPStats(tapName string) (rxBytes, txBytes uint64, err error) {
	return 0, 0, fmt.Errorf("read TAP stats: not supported on macOS")
}
