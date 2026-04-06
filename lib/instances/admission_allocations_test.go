package instances

import (
	"context"
	"testing"

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
