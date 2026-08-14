//go:build linux

package instances

import (
	"bufio"
	"context"
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

	"github.com/kernel/hypeman/lib/hypervisor"
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

	resolved, err := resolveLiveHypervisorPID(HypervisorProcessIdentity{HypervisorPID: &pid, HypervisorStartTime: startTime, HypervisorBootID: hostBootID()}, filepath.Join(t.TempDir(), "missing.sock"))
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

func TestKillHypervisorSucceedsOnIdentityFromDifferentBoot(t *testing.T) {
	process := exec.Command("sleep", "30")
	require.NoError(t, process.Start())
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})

	pid := process.Process.Pid
	startTime := processStartTime(pid)
	require.NotZero(t, startTime)

	// An identity recorded under a different host boot proves the recorded
	// hypervisor is gone: the kill must succeed as a no-op so delete can
	// proceed, without signaling whatever process wears the PID now.
	m := &manager{}
	require.NoError(t, m.killHypervisor(context.Background(), &Instance{
		StoredMetadata: StoredMetadata{
			Id: "kill-test",
			HypervisorProcessIdentity: HypervisorProcessIdentity{
				HypervisorPID:       &pid,
				HypervisorStartTime: startTime,
				HypervisorBootID:    "different-boot",
			},
			SocketPath: filepath.Join(t.TempDir(), "missing.sock"),
		},
	}))
	assert.NoError(t, syscall.Kill(pid, 0), "process identity from a different boot must not be killed")
}

func TestKillHypervisorSucceedsOnMismatchedStartTimeWithNoSocketOwner(t *testing.T) {
	process := exec.Command("sleep", "30")
	require.NoError(t, process.Start())
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})

	pid := process.Process.Pid
	startTime := processStartTime(pid)
	require.NotZero(t, startTime)

	// The identity token disproves the live PID holder is the recorded
	// hypervisor, and no process owns or references the socket: the kill
	// succeeds as a no-op and must leave the PID holder untouched.
	m := &manager{}
	require.NoError(t, m.killHypervisor(context.Background(), &Instance{
		StoredMetadata: StoredMetadata{
			Id: "kill-test",
			HypervisorProcessIdentity: HypervisorProcessIdentity{
				HypervisorPID:       &pid,
				HypervisorStartTime: startTime + 1,
				HypervisorBootID:    hostBootID(),
			},
			SocketPath: filepath.Join(t.TempDir(), "missing.sock"),
		},
	}))
	assert.NoError(t, syscall.Kill(pid, 0), "process with a mismatched identity token must not be killed")
}

func TestHypervisorProcessExistsTreatsUnresolvedSocketAsAlive(t *testing.T) {
	t.Parallel()

	assert.True(t, HypervisorProcessExists(os.Getpid(), filepath.Join(t.TempDir(), "missing.sock")))
}

func TestHypervisorProcessIdentityExistsUsesStartTime(t *testing.T) {
	t.Parallel()

	startTime := processStartTime(os.Getpid())
	require.NotZero(t, startTime)
	socketPath := filepath.Join(t.TempDir(), "missing.sock")
	bootID := hostBootID()
	require.NotEmpty(t, bootID)
	assert.True(t, HypervisorProcessIdentityExists(os.Getpid(), startTime, bootID, socketPath))
	assert.False(t, HypervisorProcessIdentityExists(os.Getpid(), startTime+1, bootID, socketPath))
	assert.False(t, HypervisorProcessIdentityExists(os.Getpid(), startTime, "different-boot", socketPath))
}

func TestHypervisorProcessExistsWithReboundSocketPathHelper(t *testing.T) {
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
}

func TestHypervisorProcessExistsTreatsReboundSocketPathAsAlive(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	process := exec.Command(os.Args[0], "-test.run=^TestHypervisorProcessExistsWithReboundSocketPathHelper$")
	process.Env = append(os.Environ(), "HYPERVISOR_SOCKET_HELPER=1", "HYPERVISOR_SOCKET_PATH="+socketPath)
	stdin, err := process.StdinPipe()
	require.NoError(t, err)
	stdout, err := process.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, process.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = process.Wait()
	})

	_, err = bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.NoError(t, os.Remove(socketPath))
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	assert.True(t, HypervisorProcessExists(os.Getpid(), socketPath))
}

func TestKillHypervisorSparesReusedPIDAndKillsSocketOwner(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	owner := exec.Command(os.Args[0], "-test.run=^TestHypervisorProcessExistsWithReboundSocketPathHelper$")
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
	owner := exec.Command(os.Args[0], "-test.run=^TestHypervisorProcessExistsWithReboundSocketPathHelper$")
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

func TestClassifyResolvedHypervisorOwnerTreatsDeadCmdlineMatchAsDeath(t *testing.T) {
	const deadPID = 1<<22 - 1
	require.False(t, ProcessExists(deadPID))

	live := exec.Command("sleep", "30")
	require.NoError(t, live.Start())
	t.Cleanup(func() {
		_ = live.Process.Kill()
		_ = live.Wait()
	})

	// The resolver's command-line match exited between the scan and the
	// liveness check: no live process owns or references the socket, so the
	// recorded hypervisor is provably gone even with a live stored PID,
	// matching the ErrNoOwningProcess conclusion instead of failing closed.
	pid, err := classifyResolvedHypervisorOwner("/fake.sock", live.Process.Pid, deadPID, false, nil)
	require.NoError(t, err)
	assert.Zero(t, pid)

	pid, err = classifyResolvedHypervisorOwner("/fake.sock", 0, deadPID, false, nil)
	require.NoError(t, err)
	assert.Zero(t, pid)
}

func TestShutdownHypervisorKillsResolvedOwnerWhenClientUnavailable(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	owner := exec.Command(os.Args[0], "-test.run=^TestHypervisorProcessExistsWithReboundSocketPathHelper$")
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

func TestShutdownHypervisorFailsClosedOnUnconfirmedOwnership(t *testing.T) {
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

	stalePID := stale.Process.Pid
	m := &manager{}
	err := m.shutdownHypervisor(context.Background(), &Instance{
		StoredMetadata: StoredMetadata{
			Id:                        "shutdown-unconfirmed",
			HypervisorType:            hypervisor.TypeCloudHypervisor,
			HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &stalePID},
			SocketPath:                socketPath,
		},
	})
	require.ErrorContains(t, err, "confirm hypervisor ownership before shutdown")
	assert.NoError(t, syscall.Kill(stalePID, 0), "stored PID must not be signaled on unconfirmed ownership")
	assert.NoError(t, syscall.Kill(match.Process.Pid, 0), "command-line match must not be signaled")
	_, statErr := os.Stat(socketPath)
	assert.NoError(t, statErr, "socket must be kept as evidence for the hardened kill path")
}

func TestForceKillHypervisorProcessSucceedsWhenNoProcessOwnsSocket(t *testing.T) {
	process := exec.Command("sleep", "30")
	require.NoError(t, process.Start())
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})

	// No process owns or references the socket, so the live stored PID is a
	// recycled number: force kill succeeds as a no-op without signaling it.
	pid := process.Process.Pid
	m := &manager{}
	require.NoError(t, m.forceKillHypervisorProcess(context.Background(), &Instance{
		StoredMetadata: StoredMetadata{Id: "kill-test", HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &pid}, SocketPath: filepath.Join(t.TempDir(), "missing.sock")},
	}))
	assert.NoError(t, syscall.Kill(pid, 0), "process with a recycled PID must not be killed")
}

func TestKillProcessAndWaitIgnoresExitedProcess(t *testing.T) {
	process := exec.Command("true")
	require.NoError(t, process.Run())

	require.NoError(t, killProcessAndWait(process.Process.Pid, killEscalationWait))
}

func TestRefreshHypervisorPIDTrustsLiveStoredPID(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	owner := exec.Command(os.Args[0], "-test.run=^TestHypervisorProcessExistsWithReboundSocketPathHelper$")
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

func TestKillHypervisorSurvivesConcurrentReaper(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	process := exec.Command(os.Args[0], "-test.run=^TestHypervisorProcessExistsWithReboundSocketPathHelper$")
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

func TestKillHypervisorFailsOnUnconfirmedCommandLineMatch(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	// A process whose command line contains the socket path but that does not
	// own a listening socket: ResolveProcessPID resolves it unconfirmed.
	match := exec.Command("sh", "-c", "sleep 30", "sh", socketPath)
	require.NoError(t, match.Start())
	t.Cleanup(func() {
		_ = match.Process.Kill()
		_ = match.Wait()
	})

	matchPID := match.Process.Pid
	m := &manager{}
	require.Error(t, m.killHypervisor(context.Background(), &Instance{
		StoredMetadata: StoredMetadata{Id: "kill-test", HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &matchPID}, SocketPath: socketPath},
	}), "a command-line match must not satisfy destructive ownership verification")

	assert.NoError(t, syscall.Kill(matchPID, 0), "process matched only by command line must not be killed")
}

func TestKillHypervisorFailsOnCommandLineMatchWithNilStoredPID(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	match := exec.Command("sh", "-c", "sleep 30", "sh", socketPath)
	require.NoError(t, match.Start())
	t.Cleanup(func() {
		_ = match.Process.Kill()
		_ = match.Wait()
	})

	m := &manager{}
	require.Error(t, m.killHypervisor(context.Background(), &Instance{
		StoredMetadata: StoredMetadata{Id: "kill-test", SocketPath: socketPath},
	}), "a command-line match must fail closed without a stored PID")

	assert.NoError(t, syscall.Kill(match.Process.Pid, 0), "process matched only by command line must not be killed")
}

func TestHypervisorProcessExistsRejectsDifferentLiveSocketOwner(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	process := exec.Command("sleep", "30")
	require.NoError(t, process.Start())
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})

	assert.False(t, HypervisorProcessExists(process.Process.Pid, socketPath))
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

	t.Run("command-line-only match stores bare PID", func(t *testing.T) {
		socketPath := filepath.Join(t.TempDir(), "test.sock")
		match := exec.Command("sh", "-c", "sleep 30", "sh", socketPath)
		require.NoError(t, match.Start())
		t.Cleanup(func() {
			_ = match.Process.Kill()
			_ = match.Wait()
		})

		stored := &StoredMetadata{SocketPath: socketPath}
		pid := resolveRuntimeHypervisorPID(log, stored, deadPID)

		assert.Equal(t, match.Process.Pid, pid)
		require.NotNil(t, stored.HypervisorPID)
		assert.Equal(t, match.Process.Pid, *stored.HypervisorPID)
		assert.Zero(t, stored.HypervisorStartTime, "unconfirmed match must not mint the identity token")
		assert.Empty(t, stored.HypervisorBootID, "unconfirmed match must not mint the identity token")
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
