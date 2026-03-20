package firecracker

import (
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMapVMState(t *testing.T) {
	state, err := mapVMState(firecrackerStateNotStarted)
	require.NoError(t, err)
	assert.Equal(t, hypervisor.StateCreated, state)

	state, err = mapVMState(firecrackerStateRunning)
	require.NoError(t, err)
	assert.Equal(t, hypervisor.StateRunning, state)

	state, err = mapVMState(firecrackerStatePaused)
	require.NoError(t, err)
	assert.Equal(t, hypervisor.StatePaused, state)

	_, err = mapVMState("Shutdown")
	require.Error(t, err)
}

func TestGuestTargetBytesToMiB(t *testing.T) {
	assert.Equal(t, int64(0), guestTargetBytesToMiB(0))
	assert.Equal(t, int64(0), guestTargetBytesToMiB(-1))
	assert.Equal(t, int64(1), guestTargetBytesToMiB(1))
	assert.Equal(t, int64(1), guestTargetBytesToMiB(1024*1024))
	assert.Equal(t, int64(2), guestTargetBytesToMiB(1024*1024+1))
}
