//go:build darwin

package instances

import (
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/cloudhypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/qemu"
	"github.com/kernel/hypeman/lib/hypervisor/vz"
)

func init() {
	platformStarters[hypervisor.TypeCloudHypervisor] = cloudhypervisor.NewStarter()
	platformStarters[hypervisor.TypeQEMU] = qemu.NewStarter()
	platformStarters[hypervisor.TypeVZ] = vz.NewStarter()
}
