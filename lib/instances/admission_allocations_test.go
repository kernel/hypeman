package instances

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/resources"
	"github.com/stretchr/testify/require"
)

func TestRollbackAdmissionAllocationActiveClearsVisibleAllocation(t *testing.T) {
	t.Parallel()

	m := &manager{
		admissionAllocations:       make(map[string]resources.InstanceAllocation),
		admissionAllocationsLoaded: true,
	}
	pid := 1234
	stored := &StoredMetadata{
		Id:            "inst-1",
		Name:          "test-instance",
		Vcpus:         2,
		Size:          1024,
		HypervisorPID: &pid,
	}

	m.setAdmissionAllocationActive(stored, true)

	allocs, err := m.ListInstanceAllocations(context.Background())
	require.NoError(t, err)
	require.Len(t, allocs, 1)
	require.Equal(t, string(StateCreated), allocs[0].State)

	m.rollbackAdmissionAllocationActive(stored)

	allocs, err = m.ListInstanceAllocations(context.Background())
	require.NoError(t, err)
	require.Len(t, allocs, 1)
	require.Nil(t, stored.HypervisorPID)
	require.Equal(t, string(StateStopped), allocs[0].State)
}

func TestReconcileAdmissionAllocationsMarksMissingSocketInactive(t *testing.T) {
	t.Parallel()

	p := paths.New(t.TempDir())
	socketPath := filepath.Join(p.DataDir(), "vm.sock")
	require.NoError(t, os.WriteFile(socketPath, nil, 0o644))

	pid := 4321
	stored := StoredMetadata{
		Id:            "inst-2",
		Name:          "test-instance",
		Vcpus:         2,
		Size:          1024,
		HypervisorPID: &pid,
		SocketPath:    socketPath,
	}

	m := &manager{
		paths:                      p,
		admissionAllocations:       make(map[string]resources.InstanceAllocation),
		admissionAllocationsLoaded: true,
	}
	m.setAdmissionAllocationActive(&stored, true)
	require.NoError(t, m.ensureDirectories(stored.Id))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: stored}))

	require.NoError(t, os.Remove(socketPath))

	m.reconcileAdmissionAllocations(context.Background())

	allocs, err := m.ListInstanceAllocations(context.Background())
	require.NoError(t, err)
	require.Len(t, allocs, 1)
	require.Equal(t, string(StateStopped), allocs[0].State)
}
