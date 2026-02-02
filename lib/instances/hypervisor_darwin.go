//go:build darwin

package instances

import (
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/vz"
)

func init() {
	additionalStarters[hypervisor.TypeVZ] = vz.NewStarter()
}
