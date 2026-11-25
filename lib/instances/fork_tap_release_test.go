package instances

import (
	"context"
	"errors"
	"testing"

	"github.com/kernel/hypeman/lib/network"
	"github.com/stretchr/testify/require"
)

type failingTAPRelease struct {
	network.Manager
	ids []string
}

func (r *failingTAPRelease) ReleaseByInstanceID(_ context.Context, id string) error {
	r.ids = append(r.ids, id)
	return errors.New("TAP busy")
}

func createPendingForkWithoutAllocation(t *testing.T, m *manager, fork string) {
	t.Helper()
	savePendingForkFixture(t, m, fork, true, func(stored *StoredMetadata) { stored.NetworkEnabled = true })
	alloc, err := m.networkManager.GetAllocation(t.Context(), fork)
	require.NoError(t, err)
	require.Nil(t, alloc)
}

// The TAP reconciler deletes a TAP that no live instance claims, so a fork is
// not kept for one that could not be released.
func TestRecoveryDeletesForkWhenTAPReleaseFails(t *testing.T) {
	m, _ := setupTestManager(t)
	const fork = "fork-tap-busy"
	createPendingForkWithoutAllocation(t, m, fork)
	release := &failingTAPRelease{Manager: m.networkManager}
	m.networkManager = release

	m.RecoverInterruptedForks(t.Context())
	require.Equal(t, []string{fork}, release.ids)
	require.NoDirExists(t, m.paths.InstanceDir(fork))
}
