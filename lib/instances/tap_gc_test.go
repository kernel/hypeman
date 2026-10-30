package instances

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTAPPreserveIDs(t *testing.T) {
	for _, tt := range []struct {
		state State
		keep  bool
	}{
		{StateRunning, true},
		{StateInitializing, true},
		{StatePaused, true},
		{StateUnknown, true},
		{StateStopped, false},
		{StateStandby, false},
		{StateCreated, false},
		{StateShutdown, false},
	} {
		t.Run(string(tt.state), func(t *testing.T) {
			inst := Instance{StoredMetadata: StoredMetadata{Id: "guest"}, State: tt.state}
			ids := TAPPreserveIDs([]Instance{inst})
			if tt.keep {
				assert.Equal(t, []string{"guest"}, ids)
			} else {
				assert.Empty(t, ids)
			}
		})
	}
	assert.Empty(t, TAPPreserveIDs(nil))
}

type tapGCNetwork struct {
	network.Manager
	preserved []string
	minAge    time.Duration
}

func (n *tapGCNetwork) CleanupOrphanedTAPs(_ context.Context, ids []string, minAge time.Duration) int {
	n.preserved = ids
	n.minAge = minAge
	return 0
}

func (n *tapGCNetwork) CleanupOrphanedClasses(context.Context) int { return 0 }

func TestReconcileTAPsPreservesPausedGuest(t *testing.T) {
	net := &tapGCNetwork{}
	m := &manager{paths: paths.New(t.TempDir()), networkManager: net}
	for id, state := range map[string]hypervisor.VMState{
		"running": hypervisor.StateRunning,
		"paused":  hypervisor.StatePaused,
	} {
		require.NoError(t, m.ensureDirectories(id))
		socket := m.paths.InstanceDir(id) + "/fc.sock"
		require.NoError(t, os.WriteFile(socket, nil, 0600))
		require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
			Id: id, Name: id, DataDir: m.paths.InstanceDir(id), SocketPath: socket,
		}}))
		m.storeCachedHypervisorState(id, state)
	}

	m.reconcileTAPs(context.Background())
	assert.ElementsMatch(t, []string{"running", "paused"}, net.preserved)
	assert.Equal(t, tapGCMinAge, net.minAge)

	// Once the VMM is gone, its TAP is eligible for cleanup again.
	require.NoError(t, os.Remove(m.paths.InstanceDir("paused")+"/fc.sock"))
	m.reconcileTAPs(context.Background())
	assert.Equal(t, []string{"running"}, net.preserved)
}
