package instances

import (
	"path/filepath"

	"github.com/kernel/hypeman/lib/hypervisor"
)

func (m *manager) instanceVsockSocketPath(instanceID string, hvType hypervisor.Type) string {
	if hvType == hypervisor.TypeVZ {
		return filepath.Join(m.paths.InstanceDir(instanceID), "vz.vsock")
	}
	return m.paths.InstanceVsockSocket(instanceID)
}
