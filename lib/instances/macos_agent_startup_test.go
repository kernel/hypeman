package instances

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestMacOSAgentStartupDeadlineStopsGuest(t *testing.T) {
	p := paths.New(t.TempDir())
	booted := time.Now().UTC().Add(-time.Hour)
	now := booted
	m := &manager{paths: p, now: func() time.Time { return now }, lifecycleEvents: newLifecycleSubscribers(),
		guestAgentReadyProbe: func(context.Context, *StoredMetadata) bool { return false },
		guestAgentInfoProbe:  currentAgentInfo,
	}
	image := testMacImage()
	image.MacOS.GuestAgent = true
	for _, id := range []string{"overdue", "booting"} {
		require.NoError(t, m.ensureDirectories(id))
		socket := filepath.Join(p.InstanceDir(id), "vz.sock")
		require.NoError(t, os.WriteFile(socket, nil, 0600))
		lifecycleNoopHypervisorStates.Store(socket, hypervisor.StateRunning)
		t.Cleanup(func() { lifecycleNoopHypervisorStates.Delete(socket) })
		stored := StoredMetadata{Id: id, Name: id, DataDir: p.InstanceDir(id), SocketPath: socket, HypervisorType: lifecycleNoopHypervisorType, MacOS: image.MacOS, CreatedAt: booted}
		since := booted
		if id == "booting" {
			since = booted.Add(time.Hour - time.Minute)
		}
		stored.Phases.Record(phasetracking.PhaseInitializing, since)
		require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: stored}))
	}
	m.macOSAgentNotReady.Store("overdue", "read guest agent info: connection reset")
	now = booted.Add(time.Hour)

	m.enforceMacOSAgentStartupDeadline(context.Background(), slog.Default())

	overdue, err := m.GetInstance(context.Background(), "overdue")
	require.NoError(t, err)
	require.Equal(t, StateStopped, overdue.State)
	require.Contains(t, overdue.ExitMessage, "did not become ready within 10m0s")
	require.Contains(t, overdue.ExitMessage, "connection reset", "the last probe failure is kept")
	require.Nil(t, overdue.ExitCode)
	require.FileExists(t, filepath.Join(p.InstanceDir("overdue"), "metadata.json"), "the instance and its files are kept")

	booting, err := m.GetInstance(context.Background(), "booting")
	require.NoError(t, err)
	require.Equal(t, StateInitializing, booting.State, "a guest within the deadline is left alone")
	require.Empty(t, booting.ExitMessage)
}

func TestMacOSAgentStartupOverdueRules(t *testing.T) {
	image := testMacImage()
	image.MacOS.GuestAgent = true
	now := time.Now().UTC()
	initializing := func(since time.Time) phasetracking.Tracker {
		var tr phasetracking.Tracker
		tr.Record(phasetracking.PhaseInitializing, since)
		return tr
	}
	old := now.Add(-macOSAgentStartupDeadline - time.Second)
	ready := now
	for name, tc := range map[string]struct {
		stored StoredMetadata
		want   bool
	}{
		"overdue":                {StoredMetadata{MacOS: image.MacOS, Phases: initializing(old)}, true},
		"within deadline":        {StoredMetadata{MacOS: image.MacOS, Phases: initializing(now.Add(-time.Minute))}, false},
		"agent ready":            {StoredMetadata{MacOS: image.MacOS, Phases: initializing(old), GuestAgentReadyAt: &ready}, false},
		"agent skipped":          {StoredMetadata{MacOS: image.MacOS, Phases: initializing(old), SkipGuestAgent: true}, false},
		"image without an agent": {StoredMetadata{MacOS: testMacImage().MacOS, Phases: initializing(old)}, false},
		"linux guest":            {StoredMetadata{Phases: initializing(old)}, false},
	} {
		require.Equal(t, tc.want, macOSAgentStartupOverdue(&tc.stored, now), name)
	}
}
