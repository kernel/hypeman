//go:build !linux

package images

import "context"

type dmLinearDevice struct{}

func dmLinearAvailable(context.Context) bool {
	return false
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
