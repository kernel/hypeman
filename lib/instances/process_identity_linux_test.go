//go:build linux

package instances

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveLiveHypervisorPIDWithoutStoredPID(t *testing.T) {
	t.Run("missing socket", func(t *testing.T) {
		pid, err := resolveLiveHypervisorPID(HypervisorProcessIdentity{}, filepath.Join(t.TempDir(), "missing.sock"))
		require.NoError(t, err)
		assert.Zero(t, pid)
	})

	t.Run("live owner", func(t *testing.T) {
		socketPath := filepath.Join(t.TempDir(), "test.sock")
		listener, err := net.Listen("unix", socketPath)
		require.NoError(t, err)
		defer listener.Close()

		pid, err := resolveLiveHypervisorPID(HypervisorProcessIdentity{}, socketPath)
		require.NoError(t, err)
		assert.Equal(t, os.Getpid(), pid)
	})
}

// TestResolveLiveHypervisorPIDReapsZombieChild guards against leaking one
// zombie per direct-child VMM that exits on its own: ProcessExists treats
// zombies as dead, so the confirmed-gone paths in stop, delete, and standby
// never reach the Wait4 in WaitForProcessExit.
func TestResolveLiveHypervisorPIDReapsZombieChild(t *testing.T) {
	child := exec.Command("true")
	require.NoError(t, child.Start())
	pid := child.Process.Pid

	require.Eventually(t, func() bool {
		state, err := readLinuxProcessState(pid)
		return err == nil && state == "Z"
	}, 5*time.Second, 10*time.Millisecond, "child never became a zombie")

	resolved, err := resolveLiveHypervisorPID(HypervisorProcessIdentity{HypervisorPID: &pid}, filepath.Join(t.TempDir(), "missing.sock"))
	require.NoError(t, err)
	assert.Zero(t, resolved)

	var status syscall.WaitStatus
	_, waitErr := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	assert.ErrorIs(t, waitErr, syscall.ECHILD, "zombie child was not reaped")
}

func TestHostBootIDIsStable(t *testing.T) {
	first := hostBootID()
	require.NotEmpty(t, first)
	assert.Equal(t, first, hostBootID())
}

func TestProcessStartTime(t *testing.T) {
	assert.NotZero(t, processStartTime(os.Getpid()))
	assert.Zero(t, processStartTime(0))
	assert.Zero(t, processStartTime(-1))

	const nonexistentPID = 1<<22 - 1
	require.False(t, ProcessExists(nonexistentPID))
	assert.Zero(t, processStartTime(nonexistentPID))
}

func TestResolveLiveHypervisorPIDUsesMatchingStartTime(t *testing.T) {
	process := exec.Command("sleep", "30")
	require.NoError(t, process.Start())
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})

	pid := process.Process.Pid
	startTime := processStartTime(pid)
	require.NotZero(t, startTime)

	resolved, err := resolveLiveHypervisorPID(HypervisorProcessIdentity{HypervisorPID: &pid, HypervisorStartTime: startTime, HypervisorBootID: hostBootID()}, "")
	require.NoError(t, err)
	assert.Equal(t, pid, resolved)
}

func TestKillHypervisorUsesMatchingStartTimeWhenSocketIsGone(t *testing.T) {
	process := exec.Command("sleep", "30")
	require.NoError(t, process.Start())
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})

	pid := process.Process.Pid
	startTime := processStartTime(pid)
	require.NotZero(t, startTime)
	socketPath := filepath.Join(t.TempDir(), "missing.sock")

	m := &manager{}
	require.NoError(t, m.killHypervisor(context.Background(), &Instance{
		StoredMetadata: StoredMetadata{
			Id: "kill-test",
			HypervisorProcessIdentity: HypervisorProcessIdentity{
				HypervisorPID:       &pid,
				HypervisorStartTime: startTime,
				HypervisorBootID:    hostBootID(),
			},
			SocketPath: socketPath,
		},
	}))

	assert.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH)
	_, statErr := os.Stat(socketPath)
	assert.True(t, os.IsNotExist(statErr), "instance socket should be removed")
}

// TestResolveLiveHypervisorPIDTreatsDisprovenIdentityAsDead covers the
// disproof branches: an identity token that cannot belong to the live PID
// holder, plus no socket owner, resolves to "provably dead" (0, nil) so
// stop/delete proceed without signaling the recycled PID.
func TestResolveLiveHypervisorPIDTreatsDisprovenIdentityAsDead(t *testing.T) {
	process := exec.Command("sleep", "30")
	require.NoError(t, process.Start())
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})

	pid := process.Process.Pid
	startTime := processStartTime(pid)
	require.NotZero(t, startTime)

	for name, id := range map[string]HypervisorProcessIdentity{
		"different boot":        {HypervisorPID: &pid, HypervisorStartTime: startTime, HypervisorBootID: "different-boot"},
		"mismatched start time": {HypervisorPID: &pid, HypervisorStartTime: startTime + 1, HypervisorBootID: hostBootID()},
	} {
		t.Run(name, func(t *testing.T) {
			resolved, err := resolveLiveHypervisorPID(id, "")
			require.NoError(t, err)
			assert.Zero(t, resolved)
			assert.NoError(t, syscall.Kill(pid, 0), "disproven identity must not signal the PID holder")
		})
	}
}

func TestResolveLiveHypervisorPIDFailsClosedWithoutSocketOrIdentity(t *testing.T) {
	pid := os.Getpid()
	resolved, err := resolveLiveHypervisorPID(HypervisorProcessIdentity{HypervisorPID: &pid}, "")
	require.ErrorContains(t, err, "without a socket path")
	assert.Zero(t, resolved)
}

func TestSocketListenerHelper(t *testing.T) {
	if os.Getenv("HYPERVISOR_SOCKET_HELPER") != "1" {
		return
	}

	listener, err := net.Listen("unix", os.Getenv("HYPERVISOR_SOCKET_PATH"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer listener.Close()
	fmt.Fprintln(os.Stdout, "ready")
	_, _ = os.Stdin.Read(make([]byte, 1))
	if os.Getenv("HYPERVISOR_SOCKET_CLOSE_BEFORE_EXIT") == "1" {
		_ = listener.Close()
		fmt.Fprintln(os.Stdout, "closed")
		_, _ = os.Stdin.Read(make([]byte, 1))
	}
}

func TestKillHypervisorSparesReusedPIDAndKillsSocketOwner(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	owner := exec.Command(os.Args[0], "-test.run=^TestSocketListenerHelper$")
	owner.Env = append(os.Environ(), "HYPERVISOR_SOCKET_HELPER=1", "HYPERVISOR_SOCKET_PATH="+socketPath)
	stdin, err := owner.StdinPipe()
	require.NoError(t, err)
	stdout, err := owner.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, owner.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = owner.Process.Kill()
		_ = owner.Wait()
	})
	_, err = bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)

	stale := exec.Command("sleep", "30")
	require.NoError(t, stale.Start())
	t.Cleanup(func() {
		_ = stale.Process.Kill()
		_ = stale.Wait()
	})

	stalePID := stale.Process.Pid
	m := &manager{}
	require.NoError(t, m.killHypervisor(context.Background(), &Instance{
		StoredMetadata: StoredMetadata{Id: "kill-test", HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &stalePID}, SocketPath: socketPath},
	}))

	assert.NoError(t, syscall.Kill(stalePID, 0), "unrelated process holding the stale PID must survive delete")
	assert.True(t, WaitForProcessExit(owner.Process.Pid, 5*time.Second), "socket owner should be killed")
	_, statErr := os.Stat(socketPath)
	assert.True(t, os.IsNotExist(statErr), "instance socket should be removed")
}

func TestGracefulShutdownWaitsForSocketOwnerInsteadOfExitedStoredPID(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	owner := exec.Command(os.Args[0], "-test.run=^TestSocketListenerHelper$")
	owner.Env = append(os.Environ(), "HYPERVISOR_SOCKET_HELPER=1", "HYPERVISOR_SOCKET_PATH="+socketPath)
	stdin, err := owner.StdinPipe()
	require.NoError(t, err)
	stdout, err := owner.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, owner.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = owner.Process.Kill()
		_ = owner.Wait()
	})
	_, err = bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)

	stale := exec.Command("true")
	require.NoError(t, stale.Run())
	stalePID := stale.Process.Pid
	inst := &Instance{StoredMetadata: StoredMetadata{
		Id:                        "graceful-stale-pid",
		HypervisorType:            hypervisor.TypeCloudHypervisor,
		HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &stalePID},
		SocketPath:                socketPath,
		VsockSocket:               filepath.Join(t.TempDir(), "missing-vsock.sock"),
	}}

	m := &manager{}
	assert.False(t, m.tryGracefulGuestShutdown(context.Background(), inst, 1),
		"stop and delete must fall back to the hardened kill path while the socket owner is alive")
	assert.True(t, ProcessExists(owner.Process.Pid))
}

func TestGracefulShutdownFallbackKillsCapturedPIDAfterListenerCloses(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	owner := exec.Command(os.Args[0], "-test.run=^TestSocketListenerHelper$")
	owner.Env = append(os.Environ(),
		"HYPERVISOR_SOCKET_HELPER=1",
		"HYPERVISOR_SOCKET_CLOSE_BEFORE_EXIT=1",
		"HYPERVISOR_SOCKET_PATH="+socketPath,
	)
	stdin, err := owner.StdinPipe()
	require.NoError(t, err)
	stdout, err := owner.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, owner.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = owner.Process.Kill()
		_ = owner.Wait()
	})
	output := bufio.NewReader(stdout)
	_, err = output.ReadString('\n')
	require.NoError(t, err)

	m := &manager{shutdownGuestFn: func(context.Context, hypervisor.VsockDialer, int32) error {
		if _, err := stdin.Write([]byte{1}); err != nil {
			return err
		}
		_, err := output.ReadString('\n')
		return err
	}}
	inst := &Instance{StoredMetadata: StoredMetadata{
		Id:                        "graceful-listener-close",
		HypervisorType:            hypervisor.TypeCloudHypervisor,
		HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &owner.Process.Pid},
		SocketPath:                socketPath,
		VsockSocket:               filepath.Join(t.TempDir(), "missing-vsock.sock"),
	}}

	assert.False(t, m.tryGracefulGuestShutdown(context.Background(), inst, 0))
	require.NoError(t, m.killHypervisor(context.Background(), inst))
	assert.False(t, ProcessExists(owner.Process.Pid))
}

func TestShutdownHypervisorSparesReusedPIDWhenNoProcessOwnsSocket(t *testing.T) {
	process := exec.Command("sleep", "30")
	require.NoError(t, process.Start())
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})

	// No process owns or references the socket, so the live stored PID is a
	// recycled number: shutdown must not signal it. Any returned error comes
	// from the unreachable control socket, not from ownership resolution.
	pid := process.Process.Pid
	m := &manager{}
	err := m.shutdownHypervisor(context.Background(), &Instance{
		StoredMetadata: StoredMetadata{
			Id:                        "shutdown-reused-pid",
			HypervisorType:            hypervisor.TypeCloudHypervisor,
			HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &pid},
			SocketPath:                filepath.Join(t.TempDir(), "missing.sock"),
		},
	})
	require.NotContains(t, fmt.Sprint(err), "confirm hypervisor ownership")
	assert.NoError(t, syscall.Kill(pid, 0), "process with a recycled PID must not be killed")
}

func TestShutdownHypervisorRemovesStaleSocketWhenNoLiveOwner(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "stale.sock")
	require.NoError(t, os.WriteFile(socketPath, nil, 0600))

	// No process owns or references the socket and no client factory exists
	// for this hypervisor type: the hypervisor is provably gone, so shutdown
	// must report success and remove the stale socket file.
	m := &manager{}
	require.NoError(t, m.shutdownHypervisor(context.Background(), &Instance{
		StoredMetadata: StoredMetadata{
			Id:             "shutdown-stale-socket",
			HypervisorType: hypervisor.Type("unregistered-stale-socket-test"),
			SocketPath:     socketPath,
		},
	}))
	_, statErr := os.Stat(socketPath)
	assert.True(t, os.IsNotExist(statErr), "stale socket should be removed once the hypervisor is provably gone")
}

func TestClassifyResolvedHypervisorOwner(t *testing.T) {
	const deadPID = 1<<22 - 1
	require.False(t, ProcessExists(deadPID))

	live := exec.Command("sleep", "30")
	require.NoError(t, live.Start())
	t.Cleanup(func() {
		_ = live.Process.Kill()
		_ = live.Wait()
	})
	livePID := live.Process.Pid

	// A resolved owner that exited between the scan and the liveness check is
	// the same provable-death conclusion as ErrNoOwningProcess, even with a
	// live stored PID. Only a failed scan fails closed.
	for _, tc := range []struct {
		name             string
		stored, resolved int
		err              error
		wantPID          int
		wantErr          bool
	}{
		{name: "live resolved owner", resolved: livePID, wantPID: livePID},
		{name: "dead resolved owner with live stored PID", stored: livePID, resolved: deadPID},
		{name: "dead resolved owner without stored PID", resolved: deadPID},
		{name: "no owning process with live stored PID", stored: livePID, err: fmt.Errorf("wrapped: %w", hypervisor.ErrNoOwningProcess)},
		{name: "scan failure with stored PID", stored: livePID, err: errors.New("inspect process fds: permission denied"), wantErr: true},
		{name: "scan failure without stored PID", err: errors.New("inspect process fds: permission denied"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pid, err := classifyResolvedHypervisorOwner("/fake.sock", tc.stored, tc.resolved, tc.err)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantPID, pid)
		})
	}
}

func TestShutdownHypervisorKillsResolvedOwnerWhenClientUnavailable(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	owner := exec.Command(os.Args[0], "-test.run=^TestSocketListenerHelper$")
	owner.Env = append(os.Environ(), "HYPERVISOR_SOCKET_HELPER=1", "HYPERVISOR_SOCKET_PATH="+socketPath)
	stdin, err := owner.StdinPipe()
	require.NoError(t, err)
	stdout, err := owner.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, owner.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = owner.Process.Kill()
		_ = owner.Wait()
	})
	_, err = bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)

	// No client factory exists for this hypervisor type, so getHypervisor
	// fails while the resolved socket owner is alive: shutdown must kill the
	// owner instead of reporting a completed shutdown.
	m := &manager{}
	require.NoError(t, m.shutdownHypervisor(context.Background(), &Instance{
		StoredMetadata: StoredMetadata{
			Id:             "shutdown-no-client",
			HypervisorType: hypervisor.Type("unregistered-shutdown-test"),
			SocketPath:     socketPath,
		},
	}))
	assert.True(t, WaitForProcessExit(owner.Process.Pid, 5*time.Second), "resolved socket owner must be killed when the control client is unavailable")
	_, statErr := os.Stat(socketPath)
	assert.True(t, os.IsNotExist(statErr), "instance socket should be removed")
}

func TestShutdownHypervisorIgnoresCommandLineBystander(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	require.NoError(t, os.WriteFile(socketPath, nil, 0600))
	match := exec.Command("sh", "-c", "sleep 30", "sh", socketPath)
	require.NoError(t, match.Start())
	t.Cleanup(func() {
		_ = match.Process.Kill()
		_ = match.Wait()
	})

	stale := exec.Command("sleep", "30")
	require.NoError(t, stale.Start())
	t.Cleanup(func() {
		_ = stale.Process.Kill()
		_ = stale.Wait()
	})

	// No process owns the socket listener; a debug client carrying the socket
	// path in its command line must not be mistaken for the hypervisor or
	// block shutdown.
	stalePID := stale.Process.Pid
	m := &manager{}
	err := m.shutdownHypervisor(context.Background(), &Instance{
		StoredMetadata: StoredMetadata{
			Id:                        "shutdown-cmdline-bystander",
			HypervisorType:            hypervisor.TypeCloudHypervisor,
			HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &stalePID},
			SocketPath:                socketPath,
		},
	})
	require.NotContains(t, fmt.Sprint(err), "confirm hypervisor ownership")
	assert.NoError(t, syscall.Kill(stalePID, 0), "process with a recycled PID must not be killed")
	assert.NoError(t, syscall.Kill(match.Process.Pid, 0), "command-line bystander must not be signaled")
}

func TestKillProcessAndWaitIgnoresExitedProcess(t *testing.T) {
	process := exec.Command("true")
	require.NoError(t, process.Run())

	require.NoError(t, killProcessAndWait(process.Process.Pid))
}

func TestRefreshHypervisorPIDTrustsLiveStoredPID(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	owner := exec.Command(os.Args[0], "-test.run=^TestSocketListenerHelper$")
	owner.Env = append(os.Environ(), "HYPERVISOR_SOCKET_HELPER=1", "HYPERVISOR_SOCKET_PATH="+socketPath)
	stdin, err := owner.StdinPipe()
	require.NoError(t, err)
	stdout, err := owner.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, owner.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = owner.Process.Kill()
		_ = owner.Wait()
	})
	_, err = bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)

	stale := exec.Command("sleep", "30")
	require.NoError(t, stale.Start())
	t.Cleanup(func() {
		_ = stale.Process.Kill()
		_ = stale.Wait()
	})

	// Hydration is display-only and must stay cheap: a live stored PID is
	// trusted without resolving the socket, even when another process owns
	// it. Destructive paths re-resolve through resolveLiveHypervisorPID.
	stalePID := stale.Process.Pid
	stored := StoredMetadata{HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &stalePID}, SocketPath: socketPath}
	refreshHypervisorPID(&stored, StateRunning)
	require.NotNil(t, stored.HypervisorPID)
	assert.Equal(t, stalePID, *stored.HypervisorPID)
}

func TestRefreshHypervisorPIDResolvesSocketOwnerWhenStoredPIDIsDead(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	const deadPID = 1<<22 - 1
	require.False(t, ProcessExists(deadPID))
	storedPID := deadPID
	stored := StoredMetadata{HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &storedPID}, SocketPath: socketPath}
	refreshHypervisorPID(&stored, StateRunning)

	require.NotNil(t, stored.HypervisorPID)
	assert.Equal(t, os.Getpid(), *stored.HypervisorPID)
	assert.NotZero(t, stored.HypervisorStartTime, "confirmed socket owner mints the identity token")
	assert.Equal(t, hostBootID(), stored.HypervisorBootID)
}

func TestVGPUAssignmentClaimedByLiveInstanceProtectsReusedPIDClaim(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	owner := exec.Command(os.Args[0], "-test.run=^TestSocketListenerHelper$")
	owner.Env = append(os.Environ(), "HYPERVISOR_SOCKET_HELPER=1", "HYPERVISOR_SOCKET_PATH="+socketPath)
	stdin, err := owner.StdinPipe()
	require.NoError(t, err)
	stdout, err := owner.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, owner.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = owner.Process.Kill()
		_ = owner.Wait()
	})
	_, err = bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)

	stale := exec.Command("sleep", "30")
	require.NoError(t, stale.Start())
	t.Cleanup(func() {
		_ = stale.Process.Kill()
		_ = stale.Wait()
	})

	m := &manager{paths: paths.New(t.TempDir())}
	const devicePath = "/sys/bus/pci/devices/0000:82:00.4"
	stalePID := stale.Process.Pid
	require.NoError(t, m.ensureDirectories("live-claimant"))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:                        "live-claimant",
		GPUFramework:              devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath:             devicePath,
		HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &stalePID},
		SocketPath:                socketPath,
	}}))

	claimed, err := m.vgpuAssignmentClaimedByLiveInstance(context.Background(), "other-instance", devicePath)
	require.NoError(t, err)
	assert.True(t, claimed)
}

func TestKillHypervisorSurvivesConcurrentReaper(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	process := exec.Command(os.Args[0], "-test.run=^TestSocketListenerHelper$")
	process.Env = append(os.Environ(), "HYPERVISOR_SOCKET_HELPER=1", "HYPERVISOR_SOCKET_PATH="+socketPath)
	stdin, err := process.StdinPipe()
	require.NoError(t, err)
	stdout, err := process.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, process.Start())
	_, err = bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)

	pid := process.Process.Pid
	waitDone := make(chan error, 1)
	go func() { waitDone <- process.Wait() }()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = process.Process.Kill()
		<-waitDone
	})

	m := &manager{}
	require.NoError(t, m.killHypervisor(context.Background(), &Instance{
		StoredMetadata: StoredMetadata{Id: "kill-test", HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &pid}, SocketPath: socketPath},
	}))
}

func TestKillHypervisorSucceedsOnReusedPIDWhenNoProcessOwnsSocket(t *testing.T) {
	stale := exec.Command("sleep", "30")
	require.NoError(t, stale.Start())
	t.Cleanup(func() {
		_ = stale.Process.Kill()
		_ = stale.Wait()
	})

	// Legacy metadata: live stored PID, no boot-scoped identity, and no
	// process anywhere owns or references the socket. That disproves the
	// recorded hypervisor is alive, so the kill must succeed as a no-op
	// instead of wedging stop/delete, while the PID holder stays untouched.
	stalePID := stale.Process.Pid
	socketPath := filepath.Join(t.TempDir(), "missing.sock")
	m := &manager{}
	require.NoError(t, m.killHypervisor(context.Background(), &Instance{
		StoredMetadata: StoredMetadata{Id: "kill-test", HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &stalePID}, SocketPath: socketPath},
	}))

	assert.NoError(t, syscall.Kill(stalePID, 0), "process with a recycled PID must not be killed")
}

func TestKillHypervisorIgnoresCommandLineBystander(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	// A process whose command line contains the socket path but that does not
	// own a listening socket (e.g. a debug client like ch-remote) is invisible
	// to resolution: no listener exists, so the recorded hypervisor is
	// provably gone and the kill completes without signaling the bystander.
	match := exec.Command("sh", "-c", "sleep 30", "sh", socketPath)
	require.NoError(t, match.Start())
	t.Cleanup(func() {
		_ = match.Process.Kill()
		_ = match.Wait()
	})

	matchPID := match.Process.Pid
	m := &manager{}
	require.NoError(t, m.killHypervisor(context.Background(), &Instance{
		StoredMetadata: StoredMetadata{Id: "kill-test", HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &matchPID}, SocketPath: socketPath},
	}))

	assert.NoError(t, syscall.Kill(matchPID, 0), "command-line bystander must not be killed")
}

func TestResolveRuntimeHypervisorPIDMintsIdentityOnlyWhenConfirmed(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	const deadPID = 1<<22 - 1
	require.False(t, ProcessExists(deadPID))

	t.Run("live direct child", func(t *testing.T) {
		child := exec.Command("sleep", "30")
		require.NoError(t, child.Start())
		t.Cleanup(func() {
			_ = child.Process.Kill()
			_ = child.Wait()
		})

		stored := &StoredMetadata{SocketPath: filepath.Join(t.TempDir(), "missing.sock")}
		pid := resolveRuntimeHypervisorPID(log, stored, child.Process.Pid)

		assert.Equal(t, child.Process.Pid, pid)
		require.NotNil(t, stored.HypervisorPID)
		assert.Equal(t, child.Process.Pid, *stored.HypervisorPID)
		assert.NotZero(t, stored.HypervisorStartTime)
		assert.NotEmpty(t, stored.HypervisorBootID)
	})

	t.Run("confirmed socket owner", func(t *testing.T) {
		socketPath := filepath.Join(t.TempDir(), "test.sock")
		listener, err := net.Listen("unix", socketPath)
		require.NoError(t, err)
		defer listener.Close()

		stored := &StoredMetadata{SocketPath: socketPath}
		pid := resolveRuntimeHypervisorPID(log, stored, deadPID)

		assert.Equal(t, os.Getpid(), pid)
		require.NotNil(t, stored.HypervisorPID)
		assert.Equal(t, os.Getpid(), *stored.HypervisorPID)
		assert.NotZero(t, stored.HypervisorStartTime)
		assert.NotEmpty(t, stored.HypervisorBootID)
	})

	t.Run("command-line bystander is not adopted", func(t *testing.T) {
		socketPath := filepath.Join(t.TempDir(), "test.sock")
		match := exec.Command("sh", "-c", "sleep 30", "sh", socketPath)
		require.NoError(t, match.Start())
		t.Cleanup(func() {
			_ = match.Process.Kill()
			_ = match.Wait()
		})

		stored := &StoredMetadata{SocketPath: socketPath}
		pid := resolveRuntimeHypervisorPID(log, stored, deadPID)

		assert.Equal(t, deadPID, pid, "a process matching only by command line must not be resolved")
		require.NotNil(t, stored.HypervisorPID)
		assert.Equal(t, deadPID, *stored.HypervisorPID)
		assert.Zero(t, stored.HypervisorStartTime, "a dead fallback must not mint the identity token")
		assert.Empty(t, stored.HypervisorBootID, "a dead fallback must not mint the identity token")
	})

	t.Run("dead fallback with unresolvable socket stores bare PID", func(t *testing.T) {
		stored := &StoredMetadata{SocketPath: filepath.Join(t.TempDir(), "missing.sock")}
		pid := resolveRuntimeHypervisorPID(log, stored, deadPID)

		assert.Equal(t, deadPID, pid)
		require.NotNil(t, stored.HypervisorPID)
		assert.Equal(t, deadPID, *stored.HypervisorPID)
		assert.Zero(t, stored.HypervisorStartTime, "a dead fallback must not mint the identity token")
		assert.Empty(t, stored.HypervisorBootID, "a dead fallback must not mint the identity token")
	})
}

func startTrapProcess(t *testing.T, trapAction string) (int, HypervisorProcessIdentity) {
	t.Helper()
	script := fmt.Sprintf("trap '%s' TERM; echo ready; sleep 30 & wait", trapAction)
	process := exec.Command("sh", "-c", script)
	stdout, err := process.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, process.Start())
	_, err = bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	waitDone := make(chan error, 1)
	go func() { waitDone <- process.Wait() }()
	t.Cleanup(func() {
		_ = process.Process.Kill()
		<-waitDone
	})

	pid := process.Process.Pid
	startTime := processStartTime(pid)
	require.NotZero(t, startTime)
	return pid, HypervisorProcessIdentity{HypervisorPID: &pid, HypervisorStartTime: startTime, HypervisorBootID: hostBootID()}
}

func TestKillHypervisorSIGTERMsVGPUHypervisor(t *testing.T) {
	markerPath := filepath.Join(t.TempDir(), "terminated")
	pid, identity := startTrapProcess(t, "touch "+markerPath+"; exit 0")
	socketPath := filepath.Join(t.TempDir(), "missing.sock")

	m := &manager{}
	require.NoError(t, m.killHypervisor(context.Background(), &Instance{
		State: StateInitializing,
		StoredMetadata: StoredMetadata{
			Id:                        "kill-test",
			GPUProfile:                "NVIDIA L40S-1Q",
			HypervisorProcessIdentity: identity,
			SocketPath:                socketPath,
		},
	}))

	require.Eventually(t, func() bool {
		return syscall.Kill(pid, 0) == syscall.ESRCH
	}, 5*time.Second, 10*time.Millisecond)
	assert.FileExists(t, markerPath, "hypervisor must be given SIGTERM, not SIGKILL, during vGPU driver init")
}

func TestKillHypervisorEscalatesToSIGKILLWhenSIGTERMIgnored(t *testing.T) {
	pid, identity := startTrapProcess(t, "")
	socketPath := filepath.Join(t.TempDir(), "missing.sock")

	m := &manager{vgpuInitTermGrace: 50 * time.Millisecond}
	require.NoError(t, m.killHypervisor(context.Background(), &Instance{
		State: StateInitializing,
		StoredMetadata: StoredMetadata{
			Id:                        "kill-test",
			GPUProfile:                "NVIDIA L40S-1Q",
			HypervisorProcessIdentity: identity,
			SocketPath:                socketPath,
		},
	}))

	assert.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH, "SIGTERM-ignoring hypervisor must still be hard-killed")
}

func TestKillHypervisorHardKillsNonVGPUHypervisor(t *testing.T) {
	markerPath := filepath.Join(t.TempDir(), "terminated")
	pid, identity := startTrapProcess(t, "touch "+markerPath+"; exit 0")
	socketPath := filepath.Join(t.TempDir(), "missing.sock")

	m := &manager{}
	require.NoError(t, m.killHypervisor(context.Background(), &Instance{
		State: StateRunning,
		StoredMetadata: StoredMetadata{
			Id:                        "kill-test",
			HypervisorProcessIdentity: identity,
			SocketPath:                socketPath,
		},
	}))

	assert.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH)
	assert.NoFileExists(t, markerPath, "non-vGPU hypervisors keep the direct SIGKILL path")
}
