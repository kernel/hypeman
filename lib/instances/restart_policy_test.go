package instances

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	restartpolicy "github.com/kernel/hypeman/lib/restart-policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateUpdateInstanceRequestAllowsRestartPolicyOnly(t *testing.T) {
	err := validateUpdateInstanceRequest(&metadata{}, UpdateInstanceRequest{
		RestartPolicy:    &restartpolicy.Policy{Policy: restartpolicy.PolicyAlways},
		RestartPolicySet: true,
	})

	require.NoError(t, err)
}

func TestNormalizeRestartPolicyWrapsInvalidRequest(t *testing.T) {
	_, err := normalizeRestartPolicy(&restartpolicy.Policy{
		Policy:  restartpolicy.PolicyOnFailure,
		Backoff: "0s",
	})

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidRequest))
}

func TestRestartStatusAfterPolicyUpdatePreservesManualStop(t *testing.T) {
	status := restartStatusAfterPolicyUpdate(restartpolicy.Status{
		Attempts:      3,
		BlockedReason: restartpolicy.BlockedReasonManualStop,
	})

	assert.Equal(t, restartpolicy.BlockedReasonManualStop, status.BlockedReason)
	assert.Zero(t, status.Attempts)
}

func TestRestartStatusAfterPolicyUpdateClearsRetryState(t *testing.T) {
	status := restartStatusAfterPolicyUpdate(restartpolicy.Status{
		Attempts:      3,
		BlockedReason: restartpolicy.BlockedReasonMaxAttemptsExceeded,
	})

	assert.True(t, status.IsZero())
}

func TestShouldResetRestartAttempts(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-11 * time.Minute)

	reset := shouldResetRestartAttempts(
		&restartpolicy.Policy{Policy: restartpolicy.PolicyAlways, StableAfter: "10m"},
		restartpolicy.Status{Attempts: 2},
		&Instance{
			State:          StateRunning,
			StoredMetadata: StoredMetadata{StartedAt: &startedAt},
		},
		now,
	)

	assert.True(t, reset)
}

func TestPrepareRestartAttemptPreservesLastReason(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nextStatus, reason, shouldAttempt := prepareRestartAttempt(
		&restartpolicy.Policy{Policy: restartpolicy.PolicyAlways},
		restartpolicy.Status{LastReason: restartpolicy.RestartReasonHealthCheckFailed},
		now,
	)

	require.True(t, shouldAttempt)
	assert.Equal(t, restartpolicy.RestartReasonHealthCheckFailed, reason)
	assert.Equal(t, restartpolicy.RestartReasonHealthCheckFailed, nextStatus.LastReason)
}

func TestMarkRestartManualStopLockedSkipsInstancesWithoutPolicy(t *testing.T) {
	manager, _ := setupTestManager(t)
	id := "restart-no-policy"
	require.NoError(t, manager.ensureDirectories(id))
	require.NoError(t, manager.saveMetadata(&metadata{
		StoredMetadata: StoredMetadata{
			Id:         id,
			Name:       id,
			CreatedAt:  time.Now(),
			DataDir:    manager.paths.InstanceDir(id),
			SocketPath: manager.paths.InstanceSocket(id, "cloud-hypervisor.sock"),
		},
	}))

	require.NoError(t, manager.markRestartManualStopLocked(context.Background(), id))

	loaded, err := manager.loadMetadata(id)
	require.NoError(t, err)
	assert.True(t, loaded.RestartStatus.IsZero())
}

func TestStartInstanceClearsRestartStatusWhenAlreadyRunning(t *testing.T) {
	manager, _ := setupTestManager(t)
	id := "restart-running-clear"
	require.NoError(t, manager.ensureDirectories(id))

	now := time.Now().UTC()
	socketPath := manager.paths.InstanceSocket(id, "cloud-hypervisor.sock")
	require.NoError(t, os.WriteFile(socketPath, nil, 0o600))
	manager.storeCachedHypervisorState(id, hypervisor.StateRunning)
	require.NoError(t, manager.saveMetadata(&metadata{
		StoredMetadata: StoredMetadata{
			Id:                id,
			Name:              id,
			CreatedAt:         now,
			StartedAt:         &now,
			ProgramStartedAt:  &now,
			GuestAgentReadyAt: &now,
			DataDir:           manager.paths.InstanceDir(id),
			SocketPath:        socketPath,
			HypervisorType:    hypervisor.TypeCloudHypervisor,
			RestartStatus: restartpolicy.Status{
				Attempts:      2,
				BlockedReason: restartpolicy.BlockedReasonMaxAttemptsExceeded,
				LastReason:    restartpolicy.RestartReasonHealthCheckFailed,
			},
		},
	}))

	inst, err := manager.StartInstance(context.Background(), id, StartInstanceRequest{})
	require.NoError(t, err)
	require.Equal(t, StateRunning, inst.State)
	assert.True(t, inst.RestartStatus.IsZero())

	loaded, err := manager.loadMetadata(id)
	require.NoError(t, err)
	assert.True(t, loaded.RestartStatus.IsZero())
}

func TestStartInstanceForRestartPolicyPreservesConcurrentManualStop(t *testing.T) {
	manager, _ := setupTestManager(t)
	id := "restart-manual-race"
	require.NoError(t, manager.ensureDirectories(id))

	now := time.Now().UTC()
	policy := &restartpolicy.Policy{
		Policy:      restartpolicy.PolicyAlways,
		Backoff:     "1s",
		StableAfter: "10m",
	}
	require.NoError(t, manager.saveMetadata(&metadata{
		StoredMetadata: StoredMetadata{
			Id:            id,
			Name:          id,
			CreatedAt:     now,
			DataDir:       manager.paths.InstanceDir(id),
			SocketPath:    manager.paths.InstanceSocket(id, "cloud-hypervisor.sock"),
			RestartPolicy: policy,
			RestartStatus: restartpolicy.Status{
				BlockedReason: restartpolicy.BlockedReasonManualStop,
			},
		},
	}))

	err := manager.startInstanceForRestartPolicy(
		context.Background(),
		id,
		policy,
		restartpolicy.Status{},
		now,
		slog.Default(),
	)
	require.NoError(t, err)

	loaded, err := manager.loadMetadata(id)
	require.NoError(t, err)
	assert.Equal(t, restartpolicy.BlockedReasonManualStop, loaded.RestartStatus.BlockedReason)
	assert.Zero(t, loaded.RestartStatus.Attempts)
	assert.Nil(t, loaded.StartedAt)
}

func TestStartInstanceForRestartPolicySkipsConcurrentStart(t *testing.T) {
	manager, _ := setupTestManager(t)
	id := "restart-concurrent-start"
	require.NoError(t, manager.ensureDirectories(id))

	now := time.Now().UTC()
	socketPath := manager.paths.InstanceSocket(id, "cloud-hypervisor.sock")
	require.NoError(t, os.WriteFile(socketPath, nil, 0o600))
	manager.storeCachedHypervisorState(id, hypervisor.StateRunning)
	policy := &restartpolicy.Policy{
		Policy:      restartpolicy.PolicyAlways,
		Backoff:     "1s",
		StableAfter: "10m",
	}
	require.NoError(t, manager.saveMetadata(&metadata{
		StoredMetadata: StoredMetadata{
			Id:                id,
			Name:              id,
			CreatedAt:         now,
			StartedAt:         &now,
			ProgramStartedAt:  &now,
			GuestAgentReadyAt: &now,
			DataDir:           manager.paths.InstanceDir(id),
			SocketPath:        socketPath,
			HypervisorType:    hypervisor.TypeCloudHypervisor,
			RestartPolicy:     policy,
		},
	}))

	err := manager.startInstanceForRestartPolicy(
		context.Background(),
		id,
		policy,
		restartpolicy.Status{},
		now,
		slog.Default(),
	)
	require.NoError(t, err)

	loaded, err := manager.loadMetadata(id)
	require.NoError(t, err)
	assert.True(t, loaded.RestartStatus.IsZero())
}

func TestReconcileRestartPolicyInstanceIDUsesCurrentState(t *testing.T) {
	manager, _ := setupTestManager(t)
	id := "restart-current-state"
	require.NoError(t, manager.ensureDirectories(id))

	now := time.Now().UTC()
	socketPath := manager.paths.InstanceSocket(id, "cloud-hypervisor.sock")
	require.NoError(t, os.WriteFile(socketPath, nil, 0o600))
	manager.storeCachedHypervisorState(id, hypervisor.StateRunning)

	status := restartpolicy.Status{
		Attempts:   1,
		LastReason: restartpolicy.RestartReasonHealthCheckFailed,
	}
	require.NoError(t, manager.saveMetadata(&metadata{
		StoredMetadata: StoredMetadata{
			Id:                id,
			Name:              id,
			CreatedAt:         now,
			StartedAt:         &now,
			ProgramStartedAt:  &now,
			GuestAgentReadyAt: &now,
			DataDir:           manager.paths.InstanceDir(id),
			SocketPath:        socketPath,
			HypervisorType:    hypervisor.TypeCloudHypervisor,
			RestartPolicy: &restartpolicy.Policy{
				Policy:      restartpolicy.PolicyOnFailure,
				Backoff:     "1s",
				MaxAttempts: 1,
				StableAfter: "10m",
			},
			RestartStatus: status,
		},
	}))

	err := manager.reconcileRestartPolicyInstanceID(context.Background(), id, slog.Default())
	require.NoError(t, err)

	loaded, err := manager.loadMetadata(id)
	require.NoError(t, err)
	assert.Equal(t, status.Attempts, loaded.RestartStatus.Attempts)
	assert.Equal(t, status.LastReason, loaded.RestartStatus.LastReason)
	assert.Empty(t, loaded.RestartStatus.BlockedReason)
}
