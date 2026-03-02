package firecracker

import (
	"context"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareFork_NoSnapshotPathIsSupported(t *testing.T) {
	starter := NewStarter()
	result, err := starter.PrepareFork(context.Background(), hypervisor.ForkPrepareRequest{})
	require.NoError(t, err)
	assert.False(t, result.VsockCIDUpdated)
}

func TestPrepareFork_SnapshotRewriteNotSupported(t *testing.T) {
	starter := NewStarter()
	_, err := starter.PrepareFork(context.Background(), hypervisor.ForkPrepareRequest{
		SnapshotConfigPath: "/tmp/snapshot-latest/config.json",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, hypervisor.ErrNotSupported)
}
