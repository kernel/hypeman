//go:build !linux

package images

import "context"

type dmLinearDevice struct{}

func dmLinearAvailable(context.Context) bool {
	return false
}

func existingDMLinearDevice(context.Context, string, int) (*dmLinearDevice, bool, error) {
	return nil, false, errFsmergeUnsupported
}

func listDMDeviceNames(context.Context, string) ([]string, error) {
	return nil, errFsmergeUnsupported
}

func listDMLinearLoopDependencies(context.Context) (map[string]struct{}, error) {
	return nil, errFsmergeUnsupported
}

func listLoopBackingFiles() (map[string]string, error) {
	return nil, errFsmergeUnsupported
}

func createDMLinearDevice(context.Context, string, []string) (*dmLinearDevice, error) {
	return nil, errFsmergeUnsupported
}

func (d *dmLinearDevice) Path() string {
	return ""
}

func (d *dmLinearDevice) Close() error {
	return nil
}
