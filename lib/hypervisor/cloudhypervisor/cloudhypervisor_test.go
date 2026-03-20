package cloudhypervisor

import (
	"testing"

	"github.com/kernel/hypeman/lib/vmm"
	"github.com/stretchr/testify/assert"
)

func TestAssignedGuestMemoryBytes(t *testing.T) {
	t.Run("uses configured memory without hotplug state", func(t *testing.T) {
		info := &vmm.VmInfo{
			Config: vmm.VmConfig{
				Memory:  &vmm.MemoryConfig{Size: 512},
				Balloon: &vmm.BalloonConfig{Size: 64},
			},
		}

		assert.Equal(t, int64(512), assignedGuestMemoryBytes(info))
	})

	t.Run("includes hotplugged memory via actual-plus-balloon", func(t *testing.T) {
		actual := int64(640)
		info := &vmm.VmInfo{
			Config: vmm.VmConfig{
				Memory:  &vmm.MemoryConfig{Size: 512},
				Balloon: &vmm.BalloonConfig{Size: 128},
			},
			MemoryActualSize: &actual,
		}

		assert.Equal(t, int64(768), assignedGuestMemoryBytes(info))
	})
}
