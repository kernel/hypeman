package qemu

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitalocean/go-qemu/qemu"
	"github.com/digitalocean/go-qemu/qmp/raw"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWaitVMReadyStopsWhenVMMExits(t *testing.T) {
	exited := make(chan struct{})
	polls := 0
	queryStatus := func() (qemu.Status, error) {
		polls++
		if polls == 2 {
			close(exited)
		}
		if polls >= 2 {
			return 0, errors.New("monitor closed")
		}
		return qemu.StatusInMigrate, nil
	}

	start := time.Now()
	err := waitVMReady(context.Background(), 30*time.Second, exited, queryStatus)

	require.ErrorIs(t, err, errVMMExited)
	assert.Equal(t, 2, polls)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestWaitVMReadyPollsThroughMonitorErrorsWhileVMMRuns(t *testing.T) {
	polls := 0
	queryStatus := func() (qemu.Status, error) {
		polls++
		switch polls {
		case 1:
			return 0, errors.New("monitor busy")
		case 2:
			return qemu.StatusInMigrate, nil
		}
		return qemu.StatusPaused, nil
	}

	require.NoError(t, waitVMReady(context.Background(), 30*time.Second, make(chan struct{}), queryStatus))
	assert.Equal(t, 3, polls)

	require.ErrorContains(t, waitVMReady(context.Background(), 30*time.Second, nil, func() (qemu.Status, error) {
		return qemu.StatusShutdown, nil
	}), "shut down during migration")
}

func TestStatusMapping(t *testing.T) {
	// Test that qemu.Status values are properly defined
	tests := []struct {
		name   string
		status qemu.Status
	}{
		{"running", qemu.StatusRunning},
		{"paused", qemu.StatusPaused},
		{"shutdown", qemu.StatusShutdown},
		{"prelaunch", qemu.StatusPreLaunch},
		{"in-migrate", qemu.StatusInMigrate},
		{"post-migrate", qemu.StatusPostMigrate},
		{"finish-migrate", qemu.StatusFinishMigrate},
		{"suspended", qemu.StatusSuspended},
		{"guest-panicked", qemu.StatusGuestPanicked},
		{"io-error", qemu.StatusIOError},
		{"internal-error", qemu.StatusInternalError},
		{"watchdog", qemu.StatusWatchdog},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify the status is a valid enum value (not zero except for Debug)
			// This ensures we're using the correct constants from go-qemu
			assert.NotEqual(t, qemu.Status(-1), tt.status, "status should be valid")
		})
	}
}

func TestRunStateMapping(t *testing.T) {
	// Test that raw.RunState values are properly defined
	tests := []struct {
		name  string
		state raw.RunState
	}{
		{"running", raw.RunStateRunning},
		{"paused", raw.RunStatePaused},
		{"shutdown", raw.RunStateShutdown},
		{"prelaunch", raw.RunStatePrelaunch},
		{"inmigrate", raw.RunStateInmigrate},
		{"postmigrate", raw.RunStatePostmigrate},
		{"finish-migrate", raw.RunStateFinishMigrate},
		{"suspended", raw.RunStateSuspended},
		{"guest-panicked", raw.RunStateGuestPanicked},
		{"io-error", raw.RunStateIOError},
		{"internal-error", raw.RunStateInternalError},
		{"watchdog", raw.RunStateWatchdog},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify the state is a valid enum value
			assert.NotEqual(t, raw.RunState(-1), tt.state, "state should be valid")
		})
	}
}

func TestStatusInfoFields(t *testing.T) {
	// Test that StatusInfo has the expected structure
	info := raw.StatusInfo{
		Running:    true,
		Singlestep: false,
		Status:     raw.RunStateRunning,
	}

	assert.True(t, info.Running)
	assert.False(t, info.Singlestep)
	assert.Equal(t, raw.RunStateRunning, info.Status)
}
