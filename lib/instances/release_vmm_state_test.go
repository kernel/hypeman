package instances

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
)

const releaseVMMStateTestHypervisorType hypervisor.Type = "release-vmm-state-test"

var releasedSockets sync.Map

func init() {
	hypervisor.RegisterClientFactory(releaseVMMStateTestHypervisorType, func(socketPath string) (hypervisor.Hypervisor, error) {
		state, ok := lifecycleNoopHypervisorStates.Load(socketPath)
		if !ok {
			return nil, errors.New("missing fake hypervisor state")
		}
		return lifecycleNoopHypervisor{state: state.(hypervisor.VMState)}, nil
	})
	hypervisor.RegisterVsockDialerFactory(releaseVMMStateTestHypervisorType, func(socketPath string, _ int64) hypervisor.VsockDialer {
		return lifecycleNoopVsockDialer{socketPath: socketPath}
	})
	hypervisor.RegisterVMMStateReleaser(releaseVMMStateTestHypervisorType, func(socketPath string) {
		releasedSockets.Store(socketPath, true)
	})
}

// What the hypervisor caches for a deleted or stopped VM must be released, or
// it stays for the life of the process.
func TestDeleteAndStopReleaseVMMState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action func(context.Context, *manager, string) error
	}{
		{name: "delete", action: func(ctx context.Context, m *manager, id string) error { return m.DeleteInstance(ctx, id) }},
		{name: "stop", action: func(ctx context.Context, m *manager, id string) error {
			_, err := m.StopInstance(ctx, id)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, id := newLifecycleNoopManagerWithInstance(t, StateRunning, time.Now().UTC())
			meta, err := m.loadMetadata(id)
			require.NoError(t, err)
			meta.HypervisorType = releaseVMMStateTestHypervisorType
			require.NoError(t, m.saveMetadata(meta))

			require.NoError(t, tc.action(context.Background(), m, id))

			_, released := releasedSockets.Load(meta.SocketPath)
			require.True(t, released)
		})
	}
}
