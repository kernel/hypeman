package instances

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// deadPID returns the PID of a child that has already exited and been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	require.NoError(t, cmd.Run())
	return cmd.Process.Pid
}

func TestProcessAlive(t *testing.T) {
	require.True(t, processAlive(os.Getpid()))
	require.False(t, processAlive(deadPID(t)))
	require.False(t, processAlive(0))
}

// A test holds its lease from allocation until its bridge exists, which can take
// tens of seconds. Pruning must not drop that lease while the owner still runs.
func TestPruneStaleLeasesKeepsLeaseOfLiveOwner(t *testing.T) {
	leases := map[string]subnetLease{
		"10.210.1.0/24": {BridgeName: "hmtestnobr1", PID: os.Getpid()},
		"10.210.2.0/24": {BridgeName: "hmtestnobr2", PID: deadPID(t)},
		"10.210.3.0/24": {BridgeName: "hmtestnobr3", PID: 0},
	}

	pruneStaleLeases(leases, nil)

	require.Contains(t, leases, "10.210.1.0/24")
	require.NotContains(t, leases, "10.210.2.0/24")
	require.NotContains(t, leases, "10.210.3.0/24")
}

func TestPruneStaleLeasesKeepsRoutedLeaseWithDeadOwner(t *testing.T) {
	leases := map[string]subnetLease{
		"10.210.4.0/24": {BridgeName: "hmtestnobr4", PID: deadPID(t)},
	}
	routes := []hostRoute{{cidr: "10.210.4.0/24"}}

	pruneStaleLeases(leases, routes)

	require.Contains(t, leases, "10.210.4.0/24")
}
