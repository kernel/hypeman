package qemu

import (
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetTargetGuestMemoryBytesUsesSavedConfigOnColdStart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, saveVMConfig(dir, savedVMConfig{VMConfig: hypervisor.VMConfig{MemoryBytes: 768}}))

	hv := &QEMU{socketPath: dir + "/qemu.sock"}
	target, err := hv.GetTargetGuestMemoryBytes(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(768), target)
}
