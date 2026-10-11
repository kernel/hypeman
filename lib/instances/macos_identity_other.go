//go:build !(darwin && arm64)

package instances

import "errors"

func newMacMachineIdentifierData() ([]byte, error) {
	return nil, errors.New("macOS machine identifiers require darwin/arm64")
}
