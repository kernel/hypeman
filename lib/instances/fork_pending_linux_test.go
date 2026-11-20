//go:build linux

package instances

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A PID alone does not identify the process that was making a fork: it is
// handed out again after the process exits, and after a reboot.
func TestForkOwnerIsGoneWhenItsPIDBelongsToAnotherProcess(t *testing.T) {
	current := *currentForkOwner()
	require.NotZero(t, current.StartTime)
	require.NotEmpty(t, current.BootID)

	recycled := current
	recycled.StartTime++
	assert.True(t, recycled.gone(), "same PID, started at another time")

	beforeReboot := current
	beforeReboot.BootID = "00000000-0000-0000-0000-000000000000"
	assert.True(t, beforeReboot.gone(), "same PID and start time, recorded in an earlier boot")
}
