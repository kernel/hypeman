//go:build !linux

package instances

func scanOrphanRuntimeProcesses(string) ([]orphanRuntimeProcess, error) {
	return nil, nil
}

func terminateRuntimeProcess(int) error {
	return nil
}
