//go:build darwin

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDarwinShutdownDefersPowerOffUntilReply(t *testing.T) {
	// Inspect only: never execute a host shutdown command in a test.
	command := darwinShutdownCommand()
	require.Equal(t, "/bin/sh", command.Path)
	require.Equal(t, []string{"/bin/sh", "-c", "/bin/sleep 1; exec /sbin/shutdown -h now"}, command.Args)
	require.NotNil(t, command.SysProcAttr)
	require.True(t, command.SysProcAttr.Setpgid)
	require.Nil(t, command.Cancel, "accepted shutdown must survive RPC cancellation")
}
