//go:build linux

package instances

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const autoStandbyE2EManualEnv = "HYPEMAN_RUN_AUTO_STANDBY_E2E"

func requireAutoStandbyE2EManualRun(t *testing.T) {
	t.Helper()
	if os.Getenv(autoStandbyE2EManualEnv) != "1" {
		t.Skipf("set %s=1 to run auto-standby end-to-end integration tests", autoStandbyE2EManualEnv)
	}
}

type integrationAutoStandbyStore struct {
	manager *manager
}

func (s integrationAutoStandbyStore) ListInstances(ctx context.Context) ([]autostandby.Instance, error) {
	insts, err := s.manager.ListInstances(ctx, nil)
	if err != nil {
		return nil, err
	}

	out := make([]autostandby.Instance, 0, len(insts))
	for _, inst := range insts {
		out = append(out, autostandby.Instance{
			ID:             inst.Id,
			Name:           inst.Name,
			State:          string(inst.State),
			NetworkEnabled: inst.NetworkEnabled,
			IP:             inst.IP,
			HasVGPU:        inst.GPUProfile != "" || inst.GPUMdevUUID != "",
			AutoStandby:    inst.AutoStandby,
		})
	}
	return out, nil
}

func (s integrationAutoStandbyStore) StandbyInstance(ctx context.Context, id string) error {
	_, err := s.manager.StandbyInstance(ctx, id, StandbyInstanceRequest{})
	return err
}

func (s integrationAutoStandbyStore) SetRuntime(ctx context.Context, id string, runtime *autostandby.Runtime) error {
	return s.manager.SetAutoStandbyRuntime(ctx, id, runtime)
}

func (s integrationAutoStandbyStore) SubscribeInstanceEvents() (<-chan autostandby.InstanceEvent, func(), error) {
	src, unsub := s.manager.SubscribeLifecycleEvents(LifecycleEventConsumerAutoStandby)
	dst := make(chan autostandby.InstanceEvent, 16)
	go func() {
		defer close(dst)
		for event := range src {
			var inst *autostandby.Instance
			if event.Instance != nil {
				inst = &autostandby.Instance{
					ID:             event.Instance.Id,
					Name:           event.Instance.Name,
					State:          string(event.Instance.State),
					NetworkEnabled: event.Instance.NetworkEnabled,
					IP:             event.Instance.IP,
					HasVGPU:        event.Instance.GPUProfile != "" || event.Instance.GPUMdevUUID != "",
					AutoStandby:    event.Instance.AutoStandby,
				}
			}
			dst <- autostandby.InstanceEvent{
				Action:     autostandby.InstanceEventAction(event.Action),
				InstanceID: event.InstanceID,
				Instance:   inst,
			}
		}
	}()
	return dst, unsub, nil
}

func TestAutoStandbyCloudHypervisorActiveInboundTCP(t *testing.T) {
	requireAutoStandbyE2EManualRun(t)
	requireKVMAccess(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	mgr, _ := setupCompressionTestManagerForHypervisor(t, hypervisor.TypeCloudHypervisor)
	require.NoError(t, mgr.networkManager.Initialize(ctx, nil))
	require.NoError(t, mgr.systemManager.EnsureSystemFiles(ctx))
	createNginxImageAndWait(t, ctx, mgr.imageManager)

	connSource := autostandby.NewConntrackSource()
	if _, err := connSource.ListConnections(ctx); err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			t.Skipf("conntrack access unavailable for auto-standby e2e test; rerun as root or with CAP_NET_ADMIN: %v", err)
		}
		require.NoError(t, err)
	}

	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "auto-standby-e2e",
		Image:          integrationTestImageRef(t, "docker.io/library/nginx:alpine"),
		Size:           1024 * 1024 * 1024,
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true,
		Hypervisor:     hypervisor.TypeCloudHypervisor,
		AutoStandby: &autostandby.Policy{
			Enabled:     true,
			IdleTimeout: "3s",
		},
	})
	require.NoError(t, err)
	instanceID := inst.Id

	t.Cleanup(func() {
		logInstanceArtifactsOnFailure(t, mgr, instanceID)
		_ = deleteTestInstanceNow(context.Background(), mgr, instanceID)
	})

	inst, err = waitForInstanceState(ctx, mgr, instanceID, StateRunning, 30*time.Second)
	require.NoError(t, err)
	require.NoError(t, waitForVMReady(ctx, inst.SocketPath, 10*time.Second))
	require.NoError(t, waitForExecAgent(ctx, mgr, inst.Id, 30*time.Second))
	require.NoError(t, waitForLogMessage(ctx, mgr, inst.Id, "start worker processes", 45*time.Second))

	conn, err := dialGuestPortWithRetry(inst.IP, 80, 15*time.Second)
	require.NoError(t, err)
	defer func() {
		if conn != nil {
			_ = conn.Close()
		}
	}()

	require.Eventually(t, func() bool {
		conns, err := connSource.ListConnections(ctx)
		if err != nil {
			t.Logf("conntrack read while waiting for inbound activity failed: %v", err)
			return false
		}

		count, _, err := autostandby.ActiveInboundCount(autostandby.Instance{
			ID:             inst.Id,
			Name:           inst.Name,
			State:          string(StateRunning),
			NetworkEnabled: true,
			IP:             inst.IP,
			AutoStandby: &autostandby.Policy{
				Enabled:     true,
				IdleTimeout: "3s",
			},
		}, conns)
		if err != nil {
			t.Logf("active inbound count failed: %v", err)
			return false
		}
		return count > 0
	}, 10*time.Second, 200*time.Millisecond, "host->guest TCP connection never appeared in conntrack")

	controllerCtx, controllerCancel := context.WithCancel(ctx)
	controllerDone := make(chan error, 1)
	controller := autostandby.NewController(
		integrationAutoStandbyStore{manager: mgr},
		connSource,
		autostandby.ControllerOptions{
			Log:            slog.Default(),
			ReconnectDelay: 250 * time.Millisecond,
		},
	)
	go func() {
		controllerDone <- controller.Run(controllerCtx)
	}()
	t.Cleanup(func() {
		controllerCancel()
		select {
		case err := <-controllerDone:
			if err != nil {
				t.Logf("auto-standby controller exited with error during cleanup: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Log("timed out waiting for auto-standby controller shutdown")
		}
	})

	time.Sleep(5 * time.Second)

	current, err := mgr.GetInstance(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, StateRunning, current.State, "instance should remain running while inbound TCP connection is open")

	require.NoError(t, conn.Close())
	conn = nil

	inst, err = waitForInstanceState(ctx, mgr, instanceID, StateStandby, 45*time.Second)
	require.NoError(t, err)
	require.Equal(t, StateStandby, inst.State)
}

func dialGuestPortWithRetry(ip string, port int, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	address := net.JoinHostPort(ip, fmt.Sprintf("%d", port))
	var lastErr error

	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 1*time.Second)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("timed out dialing %s", address)
	}
	return nil, lastErr
}
