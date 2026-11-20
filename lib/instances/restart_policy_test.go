package instances

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
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

func TestShouldResetRestartAttemptsUsesLatestAttemptWhenStartedAtIsOlder(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-1 * time.Hour)
	lastAttemptAt := now.Add(-5 * time.Second)

	reset := shouldResetRestartAttempts(
		&restartpolicy.Policy{Policy: restartpolicy.PolicyOnFailure, StableAfter: "30s"},
		restartpolicy.Status{
			Attempts:      1,
			LastAttemptAt: &lastAttemptAt,
		},
		&Instance{
			State:          StateInitializing,
			StoredMetadata: StoredMetadata{StartedAt: &startedAt},
		},
		now,
	)

	assert.False(t, reset)
}

func TestShouldResetRestartAttemptsAfterLatestAttemptIsStable(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-1 * time.Hour)
	lastAttemptAt := now.Add(-31 * time.Second)

	reset := shouldResetRestartAttempts(
		&restartpolicy.Policy{Policy: restartpolicy.PolicyOnFailure, StableAfter: "30s"},
		restartpolicy.Status{
			Attempts:      1,
			LastAttemptAt: &lastAttemptAt,
		},
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

func TestReconcileInterruptedHealthCheckStop(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name                   string
		block                  restartpolicy.BlockedReason
		next                   *time.Time
		wantCleanup, wantStart bool
	}{
		{name: "resume reserved attempt", wantCleanup: true, wantStart: true},
		{name: "manual stop", block: restartpolicy.BlockedReasonManualStop},
		{name: "exhausted attempts", block: restartpolicy.BlockedReasonMaxAttemptsExceeded, wantCleanup: true},
		{name: "retry scheduled", next: new(now.Add(time.Hour)), wantCleanup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, id := newLifecycleNoopManagerWithInstance(t, StateStopped, now)
			meta, err := m.loadMetadata(id)
			require.NoError(t, err)
			meta.StopCleanupPending = true
			meta.GPUDevicePath = "/sys/bus/pci/devices/0000:82:00.4"
			meta.RestartPolicy = &restartpolicy.Policy{Policy: restartpolicy.PolicyAlways, MaxAttempts: 1}
			meta.RestartStatus = restartpolicy.Status{
				Attempts: 1, LastAttemptAt: &now, LastReason: restartpolicy.RestartReasonHealthCheckFailed,
				BlockedReason: tc.block, NextAttemptAt: tc.next,
			}
			require.NoError(t, m.saveMetadata(meta))

			// A rejected manual start must not erase the controller's pending attempt or manual-stop block.
			_, err = m.StartInstance(t.Context(), id, StartInstanceRequest{})
			require.ErrorIs(t, err, ErrInvalidState)
			m.destroyVGPU = func(context.Context, devices.VGPUAssignment) error { return nil }
			m.imageManager = readyFixtureImageManager{} // a cold start reaches image lookup and fails
			err = m.reconcileRestartPolicyInstanceID(t.Context(), id, slog.Default())
			if tc.wantStart {
				require.ErrorIs(t, err, images.ErrNotFound)
			} else {
				require.NoError(t, err)
			}
			stored, err := m.loadMetadata(id)
			require.NoError(t, err)
			require.Equal(t, tc.wantCleanup, stored.StoppedAt != nil)
			require.Equal(t, !tc.wantCleanup, stored.StopCleanupPending)
			require.Equal(t, tc.wantCleanup, stored.GPUDevicePath == "")
			require.Equal(t, 1, stored.RestartStatus.Attempts)
			if tc.wantStart {
				require.Equal(t, restartpolicy.BlockedReasonMaxAttemptsExceeded, stored.RestartStatus.BlockedReason)
			} else {
				require.True(t, restartpolicy.EqualStatus(meta.RestartStatus, stored.RestartStatus))
			}
		})
	}
}

func TestHealthRestartSurvivesCrashAfterStop(t *testing.T) {
	for _, tc := range []struct {
		name         string
		wantErr      error
		wantAttempts int
		wantBlock    restartpolicy.BlockedReason
	}{
		{"before start", images.ErrNotFound, 1, restartpolicy.BlockedReasonMaxAttemptsExceeded},
		{"after boot", nil, 1, ""},
		{"manual stop", nil, 0, restartpolicy.BlockedReasonManualStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			m, id := newLifecycleNoopManagerWithInstance(t, StateRunning, now)
			meta, err := m.loadMetadata(id)
			require.NoError(t, err)
			meta.Devices = []string{"pci-device"}
			meta.RestartPolicy = &restartpolicy.Policy{Policy: restartpolicy.PolicyAlways, MaxAttempts: 1}
			meta.RestartStatus = restartpolicy.Status{Attempts: 1, LastAttemptAt: &now, LastReason: restartpolicy.RestartReasonHealthCheckFailed}
			require.NoError(t, m.saveMetadata(meta))

			// Only the stop made it to disk. The next server owns recovery.
			_, err = m.stopInstance(t.Context(), id, StopInstanceRequest{})
			require.NoError(t, err)
			restarted := &manager{paths: m.paths, lifecycleEvents: newLifecycleSubscribers(), imageManager: readyFixtureImageManager{}}
			switch tc.name {
			case "after boot":
				writeLifecycleNoopSocket(t, meta.SocketPath, hypervisor.StateRunning)
			case "manual stop":
				_, err = restarted.StopInstance(t.Context(), id, StopInstanceRequest{})
				require.NoError(t, err)
			}
			err = restarted.reconcileRestartPolicyInstanceID(t.Context(), id, slog.Default())
			require.ErrorIs(t, err, tc.wantErr)
			require.NoError(t, restarted.reconcileRestartPolicyInstanceID(t.Context(), id, slog.Default()))
			meta, err = restarted.loadMetadata(id)
			require.NoError(t, err)
			require.False(t, meta.HealthStartPending)
			require.Equal(t, tc.wantAttempts, meta.RestartStatus.Attempts)
			require.Equal(t, tc.wantBlock, meta.RestartStatus.BlockedReason)
		})
	}
}

func TestRestartPolicyUpdateDuringFailedHealthStop(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateRunning, time.Now().UTC())
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.GPUDevicePath = "/sys/bus/pci/devices/0000:82:00.4"
	meta.RestartPolicy = &restartpolicy.Policy{Policy: restartpolicy.PolicyAlways, MaxAttempts: 1}
	require.NoError(t, m.saveMetadata(meta))
	resetErr := errors.New("reset failed")
	m.destroyVGPU = func(context.Context, devices.VGPUAssignment) error { return resetErr }
	require.ErrorIs(t, m.HandleHealthCheckUnhealthy(t.Context(), id), resetErr)

	_, err = m.UpdateInstance(t.Context(), id, UpdateInstanceRequest{RestartPolicySet: true, RestartPolicy: meta.RestartPolicy})
	assert.ErrorIs(t, err, ErrInvalidState)
	m.destroyVGPU = func(context.Context, devices.VGPUAssignment) error { return nil }
	require.NoError(t, m.reconcileRestartPolicyInstanceID(t.Context(), id, slog.Default()))
	meta, err = m.loadMetadata(id)
	require.NoError(t, err)
	assert.False(t, meta.StopCleanupPending)
	assert.Empty(t, meta.GPUDevicePath)
	assert.Equal(t, restartpolicy.BlockedReasonMaxAttemptsExceeded, meta.RestartStatus.BlockedReason)
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
