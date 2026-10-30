package network

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

// A /30 leaves exactly one guest address, 10.200.0.2, after the network address,
// gateway and broadcast.
var singleGuestNetwork = &Network{Name: "default", Subnet: "10.200.0.0/30", Gateway: "10.200.0.1"}

func ipHoldTestManager(dataDir string) *manager {
	return &manager{
		paths:              paths.New(dataDir),
		config:             &config.Config{},
		pendingAllocations: make(map[string]pendingAllocation),
	}
}

func reserveIP(t *testing.T, m *manager, id string) (string, error) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.reserveAllocationIdentityLocked(context.Background(), singleGuestNetwork, AllocateRequest{InstanceID: id, InstanceName: id})
	if err != nil {
		return "", err
	}
	delete(m.pendingAllocations, id)
	return cfg.IP, nil
}

func TestHeldIPNotReallocatedUntilHoldExpires(t *testing.T) {
	m := ipHoldTestManager(t.TempDir())
	ip, err := reserveIP(t, m, "inst-a")
	require.NoError(t, err)
	require.Equal(t, "10.200.0.2", ip)

	m.HoldReleasedIP(context.Background(), ip)
	_, err = reserveIP(t, m, "inst-b")
	require.Error(t, err, "a released IP is not handed out during its hold")

	m.ipReleases[ip] = time.Now().Add(-ipReuseHold)
	ip, err = reserveIP(t, m, "inst-b")
	require.NoError(t, err)
	require.Equal(t, "10.200.0.2", ip)
	require.Empty(t, m.ipReleases, "expired holds are pruned")
}

// Standby releases the TAP but keeps the IP, so ReleaseAllocation alone must not
// start a hold.
func TestReleaseAllocationDoesNotHoldIP(t *testing.T) {
	m := ipHoldTestManager(t.TempDir())
	require.NoError(t, m.ReleaseAllocation(context.Background(), &Allocation{InstanceID: "inst-a", IP: "10.200.0.2", TAPDevice: "hype-test0000"}))

	ip, err := reserveIP(t, m, "inst-b")
	require.NoError(t, err)
	require.Equal(t, "10.200.0.2", ip)
}

func TestHeldIPSurvivesRestart(t *testing.T) {
	dataDir := t.TempDir()
	ipHoldTestManager(dataDir).HoldReleasedIP(context.Background(), "10.200.0.2")

	_, err := reserveIP(t, ipHoldTestManager(dataDir), "inst-b")
	require.Error(t, err, "a new manager loads the persisted hold")
}

func TestPersistedHoldsExpireOrAreIgnored(t *testing.T) {
	for name, contents := range map[string]string{
		"expired": `{"10.200.0.2":"` + time.Now().Add(-ipReuseHold).Format(time.RFC3339Nano) + `"}`,
		"corrupt": `{not json`,
	} {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dataDir, ipReleasesFile), []byte(contents), 0600))

			ip, err := reserveIP(t, ipHoldTestManager(dataDir), "inst-b")
			require.NoError(t, err)
			require.Equal(t, "10.200.0.2", ip)
		})
	}
}

func TestHoldAppliesWhenPersistFails(t *testing.T) {
	m := ipHoldTestManager(filepath.Join(t.TempDir(), "missing"))
	m.HoldReleasedIP(context.Background(), "10.200.0.2")

	_, err := reserveIP(t, m, "inst-b")
	require.Error(t, err)
}
