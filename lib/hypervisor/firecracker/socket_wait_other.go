//go:build !linux

package firecracker

import "time"

func waitForSocket(path string, timeout time.Duration) error {
	return waitForSocketByPolling(path, timeout)
}
