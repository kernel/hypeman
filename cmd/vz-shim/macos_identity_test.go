//go:build darwin

package main

import (
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMacOSIdentityLifetimeLock(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	c := &shimconfig.ShimConfig{MacMachineIdentifierData: "aWRlbnRpdHk="}
	first, err := lockMacOSIdentity(c)
	require.NoError(t, err)
	defer first.Close()
	equivalent := *c
	equivalent.MacMachineIdentifierData += "\n"
	second, err := lockMacOSIdentity(&equivalent)
	require.Error(t, err)
	require.Nil(t, second)
	require.NoError(t, first.Close())
	second, err = lockMacOSIdentity(c)
	require.NoError(t, err)
	second.Close()
}
