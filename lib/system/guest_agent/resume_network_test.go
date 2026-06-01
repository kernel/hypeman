//go:build linux

package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWaitAndApplyResumeNetworkMailboxTimesOutWhenPayloadMissing(t *testing.T) {
	buf := make([]byte, resumeNetworkMailboxSize)

	err := waitAndApplyResumeNetworkMailboxWithTimeout(&guestServer{}, buf, 5*time.Millisecond)

	require.Error(t, err)
	require.Contains(t, err.Error(), "payload was not patched")
}
