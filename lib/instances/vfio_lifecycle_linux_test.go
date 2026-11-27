package instances

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanceledForkRollbackRemovesRunningVFIOGuest(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateRunning, time.Now().UTC())
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	pid, identity := startTrapProcess(t, "exit 0")
	meta.HypervisorProcessIdentity = identity
	meta.GPUDevicePath = "/sys/bus/pci/devices/0000:82:00.4"
	meta.SkipGuestAgent = true
	require.NoError(t, m.saveMetadata(meta))

	released := false
	m.destroyVGPU = func(ctx context.Context, _ devices.VGPUAssignment) error {
		assert.NoError(t, ctx.Err())
		_, bounded := ctx.Deadline()
		assert.True(t, bounded, "rollback must have its own deadline")
		assert.False(t, ProcessExists(pid), "release only after the VMM exits")
		released = true
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = m.cleanupForkInstanceOnError(ctx, id)
	assert.NoError(t, err)
	assert.False(t, ProcessExists(pid))
	assert.True(t, released)
	assert.NoDirExists(t, meta.DataDir)
}

func TestInterruptedVFIODeleteKeepsLegacySocketOwner(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateRunning, time.Now().UTC())
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.SocketPath = filepath.Join(t.TempDir(), "vmm.sock")
	pid, closeListener := startClosingSocketOwner(t, meta.SocketPath)
	meta.HypervisorPID = &pid // legacy metadata without a boot-scoped identity
	meta.Devices = []string{"dev-1"}
	meta.StopTimeout = 1
	require.NoError(t, m.saveMetadata(meta))
	m.storeCachedHypervisorState(id, hypervisor.StateRunning)
	dev := &recordingDeviceManager{}
	m.deviceManager = dev
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m.shutdownGuestFn = func(context.Context, hypervisor.VsockDialer, int32) error {
		require.NoError(t, closeListener())
		cancel()
		return nil
	}
	require.ErrorIs(t, m.DeleteInstance(ctx, id), context.Canceled)

	restarted := &manager{paths: m.paths, deviceManager: dev, lifecycleEvents: newLifecycleSubscribers()}
	all, err := restarted.ListInstances(t.Context(), nil)
	require.NoError(t, err)
	assert.Contains(t, TAPPreserveIDs(all), id, "startup TAP cleanup must preserve the live owner's TAP")
	assert.True(t, NewLivenessChecker(restarted).IsInstanceRunning(t.Context(), id), "startup PCI cleanup must preserve its attachment")
	require.ErrorIs(t, restarted.DeleteInstance(ctx, id), context.Canceled)
	require.Empty(t, dev.detached)
	require.DirExists(t, meta.DataDir)
	stored, err := restarted.loadMetadata(id)
	require.NoError(t, err)
	require.Equal(t, processStartTime(pid), stored.HypervisorStartTime)
	require.NotZero(t, stored.HypervisorStartTime)
	require.Equal(t, hostBootID(), stored.HypervisorBootID)

	dev.onMarkDetached = func() { assert.False(t, ProcessExists(pid), "device detached before its owner exited") }
	require.NoError(t, restarted.DeleteInstance(t.Context(), id))
	require.Equal(t, []string{"dev-1"}, dev.detached)
	require.NoDirExists(t, meta.DataDir)
}

type stopRetryNetwork struct {
	network.Manager
	released []string
}

func (n *stopRetryNetwork) GetAllocation(_ context.Context, id string) (*network.Allocation, error) {
	return &network.Allocation{InstanceID: id}, nil
}

func (n *stopRetryNetwork) ReleaseAllocation(_ context.Context, a *network.Allocation) error {
	n.released = append(n.released, a.InstanceID)
	return nil
}

func TestStopRetryFinishesAfterVFIOGuestExits(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateRunning, time.Now().UTC())
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	dir := t.TempDir()
	terminating, release := filepath.Join(dir, "terminating"), filepath.Join(dir, "release")
	pid, identity := startTrapProcess(t, fmt.Sprintf("rm %q; touch %q; while [ ! -f %q ]; do sleep 0.01; done; exit 0",
		meta.SocketPath, terminating, release))
	meta.HypervisorProcessIdentity = identity
	meta.NetworkEnabled = true
	meta.GPUDevicePath = "/sys/bus/pci/devices/0000:82:00.4"
	require.NoError(t, m.saveMetadata(meta))

	net := &stopRetryNetwork{}
	m.networkManager = net
	var resets []string
	m.destroyVGPU = func(_ context.Context, a devices.VGPUAssignment) error {
		assert.False(t, ProcessExists(pid))
		resets = append(resets, a.DevicePath)
		return nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	var stopErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, stopErr = m.StopInstance(ctx, id, StopInstanceRequest{Force: true})
	}()
	defer func() { cancel(); <-done }()
	require.Eventually(t, func() bool { _, err := os.Stat(terminating); return err == nil }, 5*time.Second, time.Millisecond)
	cancel()
	<-done
	require.ErrorIs(t, stopErr, context.Canceled)

	// A fresh manager sees the unfinished stop even after the socket has gone.
	restarted := &manager{paths: m.paths, networkManager: net, destroyVGPU: m.destroyVGPU, lifecycleEvents: newLifecycleSubscribers()}
	pending, err := restarted.currentInstanceWithoutHydration(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, StateShutdown, pending.State)
	ttl := time.Hour
	updated, err := restarted.UpdateInstance(t.Context(), id, UpdateInstanceRequest{TTL: &ttl})
	require.NoError(t, err)
	require.Equal(t, StateShutdown, updated.State, "a TTL update must preserve the pending stop")
	require.ErrorIs(t, restarted.DeleteInstance(ctx, id), context.Canceled)
	require.True(t, ProcessExists(pid))
	require.Empty(t, net.released)
	require.Empty(t, resets)

	require.NoError(t, os.WriteFile(release, nil, 0600))
	require.Eventually(t, func() bool { return !ProcessExists(pid) }, 5*time.Second, time.Millisecond)
	inst, err := restarted.StopInstance(t.Context(), id, StopInstanceRequest{})
	require.NoError(t, err)
	assert.Equal(t, StateStopped, inst.State)
	assert.Empty(t, TAPPreserveIDs([]Instance{*inst}))
	assert.False(t, NewLivenessChecker(restarted).IsInstanceRunning(t.Context(), id))
	stored, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.NotNil(t, stored.StoppedAt)
	assert.Nil(t, stored.HypervisorPID)
	assert.Empty(t, stored.GPUDevicePath)

	_, err = restarted.StopInstance(t.Context(), id, StopInstanceRequest{})
	require.NoError(t, err)
	assert.Equal(t, []string{id}, net.released)
	assert.Equal(t, []string{meta.GPUDevicePath}, resets)
}
