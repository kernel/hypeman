//go:build darwin && arm64

package instances

import (
	vz "github.com/Code-Hex/vz/v4"
)

// newMacMachineIdentifierData returns a fresh Apple machine identifier for a cloned guest.
func newMacMachineIdentifierData() ([]byte, error) {
	id, err := vz.NewMacMachineIdentifier()
	if err != nil {
		return nil, err
	}
	return id.DataRepresentation(), nil
}
