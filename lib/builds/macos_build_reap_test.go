package builds

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

type reapInstances struct {
	instances.Manager
	lookedUp  string
	state     instances.State
	missing   bool
	stopped   bool
	deleted   bool
	deleteErr error
}

func (r *reapInstances) GetInstance(_ context.Context, idOrName string) (*instances.Instance, error) {
	r.lookedUp = idOrName
	if r.missing {
		return nil, instances.ErrNotFound
	}
	return &instances.Instance{StoredMetadata: instances.StoredMetadata{Id: "prior"}, State: r.state}, nil
}

func (r *reapInstances) StopInstance(context.Context, string) (*instances.Instance, error) {
	r.stopped = true
	r.state = instances.StateStopped
	return &instances.Instance{State: instances.StateStopped}, nil
}

func (r *reapInstances) DeleteInstance(context.Context, string) error {
	r.deleted = true
	return r.deleteErr
}

func reapManager(t *testing.T, fake *reapInstances, prior string) *manager {
	t.Helper()
	p := paths.New(t.TempDir())
	require.NoError(t, writeMetadata(p, &buildMetadata{ID: "job", Status: StatusQueued, BuilderInstance: &prior}))
	return &manager{paths: p, instanceManager: fake, logger: slog.Default()}
}

func TestReapPriorMachineBuilderRemovesOwnedBuilderBeforeRetry(t *testing.T) {
	fake := &reapInstances{state: instances.StateRunning}
	m := reapManager(t, fake, "prior")
	require.NoError(t, m.reapPriorMachineBuilder(context.Background(), "job"))
	require.True(t, fake.stopped, "a running prior builder must be stopped")
	require.True(t, fake.deleted, "the prior builder must be deleted before a retry")
}

func TestReapPriorMachineBuilderFailsWhenDeleteFails(t *testing.T) {
	fake := &reapInstances{state: instances.StateStopped, deleteErr: errors.New("storage still open")}
	m := reapManager(t, fake, "prior")
	require.Error(t, m.reapPriorMachineBuilder(context.Background(), "job"), "a failed delete must fail the retry")
}

func TestReapPriorMachineBuilderNoopWhenAbsent(t *testing.T) {
	fake := &reapInstances{missing: true}
	m := reapManager(t, fake, "prior")
	require.NoError(t, m.reapPriorMachineBuilder(context.Background(), "job"))
	require.False(t, fake.deleted)
}

func TestReapPriorMachineBuilderFindsUnrecordedBuilderByName(t *testing.T) {
	fake := &reapInstances{state: instances.StateRunning}
	p := paths.New(t.TempDir())
	require.NoError(t, writeMetadata(p, &buildMetadata{ID: "job", Status: StatusQueued}))
	m := &manager{paths: p, instanceManager: fake, logger: slog.Default()}
	require.NoError(t, m.reapPriorMachineBuilder(context.Background(), "job"))
	require.Equal(t, machineBuilderName("job"), fake.lookedUp, "a builder created before it was recorded is found by its build-derived name")
	require.True(t, fake.deleted)
}
