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
