//go:build linux

package instances

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
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

func (s integrationAutoStandbyStore) RestoreInstance(ctx context.Context, id string) error {
	_, err := s.manager.RestoreInstance(ctx, id)
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

func setupAutoStandbyE2EInstance(t *testing.T, ctx context.Context, name string) (*manager, *autostandby.ConntrackSource, *Instance) {
	t.Helper()

	mgr, _ := setupCompressionTestManagerForHypervisor(t, hypervisor.TypeCloudHypervisor)
	require.NoError(t, mgr.networkManager.Initialize(ctx, nil))
	require.NoError(t, mgr.systemManager.EnsureSystemFiles(ctx))
	createNginxImageAndWait(t, ctx, mgr.paths, mgr.imageManager)

	connSource := autostandby.NewConntrackSource()
	if _, err := connSource.ListConnections(ctx); err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			t.Skipf("conntrack access unavailable for auto-standby e2e test; rerun as root or with CAP_NET_ADMIN: %v", err)
		}
		require.NoError(t, err)
	}

	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           name,
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

	return mgr, connSource, inst
}

func startAutoStandbyE2EController(t *testing.T, ctx context.Context, mgr *manager, connSource *autostandby.ConntrackSource) {
	t.Helper()

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
}

func requireActiveInboundEventually(t *testing.T, ctx context.Context, connSource *autostandby.ConntrackSource, inst *Instance, msg string) {
	t.Helper()

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
	}, 10*time.Second, 200*time.Millisecond, msg)
}

func TestAutoStandbyCloudHypervisorActiveInboundTCP(t *testing.T) {
	requireAutoStandbyE2EManualRun(t)
	requireKVMAccess(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	mgr, connSource, inst := setupAutoStandbyE2EInstance(t, ctx, "auto-standby-e2e")
	instanceID := inst.Id

	conn, err := dialGuestPortWithRetry(inst.IP, 80, 15*time.Second)
	require.NoError(t, err)
	defer func() {
		if conn != nil {
			_ = conn.Close()
		}
	}()

	requireActiveInboundEventually(t, ctx, connSource, inst, "host->guest TCP connection never appeared in conntrack")

	startAutoStandbyE2EController(t, ctx, mgr, connSource)

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

// TestAutoStandbyCloudHypervisorHalfOpenInboundTCP reproduces the race where a
// client dials the guest but the handshake does not complete for several
// seconds, leaving the host-side conntrack flow in SYN_SENT — what a freshly
// restored guest looks like while it faults its memory back in. That flow must
// keep the VM awake: before SYN_SENT counted as activity, the idle timer fired
// mid-handshake and put the VM into standby with the client's request in
// flight.
func TestAutoStandbyCloudHypervisorHalfOpenInboundTCP(t *testing.T) {
	requireAutoStandbyE2EManualRun(t)
	requireKVMAccess(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	mgr, connSource, inst := setupAutoStandbyE2EInstance(t, ctx, "auto-standby-e2e-halfopen")
	instanceID := inst.Id

	// Confirm the guest serves before quiescing.
	probe, err := dialGuestPortWithRetry(inst.IP, 80, 15*time.Second)
	require.NoError(t, err)
	require.NoError(t, probe.Close())

	// Drop the guest's replies in the raw table, before conntrack processes
	// them: the client's SYN reaches nginx but the SYN-ACK never makes it
	// back, so the host-side flow stays in SYN_SENT while the client
	// retransmits — the same shape as a claim hitting a freshly restored
	// guest that has not answered yet.
	matchArgs := []string{"-s", inst.IP, "-p", "tcp", "--sport", "80", "-j", "DROP"}
	require.NoError(t, runIptables(append([]string{"-t", "raw", "-I", "PREROUTING", "1"}, matchArgs...)...))
	dropActive := true
	removeDrop := func() error {
		if !dropActive {
			return nil
		}
		dropActive = false
		return runIptables(append([]string{"-t", "raw", "-D", "PREROUTING"}, matchArgs...)...)
	}
	t.Cleanup(func() {
		if err := removeDrop(); err != nil {
			t.Logf("failed to remove drop rule during cleanup: %v", err)
		}
	})

	type dialResult struct {
		conn net.Conn
		err  error
	}
	dialDone := make(chan dialResult, 1)
	go func() {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(inst.IP, "80"), 90*time.Second)
		dialDone <- dialResult{conn: conn, err: err}
	}()

	requireActiveInboundEventually(t, ctx, connSource, inst, "half-open host->guest TCP flow never counted as inbound activity")

	startAutoStandbyE2EController(t, ctx, mgr, connSource)

	// Well past the 3s idle timeout; only the half-open flow holds the VM up.
	time.Sleep(5 * time.Second)

	select {
	case res := <-dialDone:
		if res.conn != nil {
			_ = res.conn.Close()
		}
		t.Fatalf("dial completed while guest replies were dropped (err=%v); half-open scenario not established", res.err)
	default:
	}

	current, err := mgr.GetInstance(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, StateRunning, current.State, "instance must remain running while a client is mid-handshake")

	// Let the handshake complete, then confirm the normal idle path still
	// reaches standby once the connection closes.
	require.NoError(t, removeDrop())

	var conn net.Conn
	select {
	case res := <-dialDone:
		require.NoError(t, res.err)
		conn = res.conn
	case <-time.After(45 * time.Second):
		t.Fatal("connection never completed after the drop rule was removed")
	}
	require.NoError(t, conn.Close())

	inst, err = waitForInstanceState(ctx, mgr, instanceID, StateStandby, 45*time.Second)
	require.NoError(t, err)
	require.Equal(t, StateStandby, inst.State)
}

func runIptables(args ...string) error {
	out, err := exec.Command("iptables", append([]string{"-w", "5"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %v: %w: %s", args, err, out)
	}
	return nil
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
