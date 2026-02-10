//go:build linux

package instances

import (
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/cloudhypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/qemu"
)

func init() {
	platformStarters[hypervisor.TypeCloudHypervisor] = cloudhypervisor.NewStarter()
	platformStarters[hypervisor.TypeQEMU] = qemu.NewStarter()
}
