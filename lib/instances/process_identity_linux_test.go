//go:build linux

package instances

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHypervisorProcessExistsTreatsUnresolvedSocketAsAlive(t *testing.T) {
	t.Parallel()

	assert.True(t, HypervisorProcessExists(os.Getpid(), filepath.Join(t.TempDir(), "missing.sock")))
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
		StoredMetadata: StoredMetadata{Id: "kill-test", HypervisorPID: &stalePID, SocketPath: socketPath},
	}))

	assert.NoError(t, syscall.Kill(stalePID, 0), "unrelated process holding the stale PID must survive delete")
	assert.True(t, WaitForProcessExit(owner.Process.Pid, 5*time.Second), "socket owner should be killed")
	_, statErr := os.Stat(socketPath)
	assert.True(t, os.IsNotExist(statErr), "instance socket should be removed")
}

func TestForceKillHypervisorProcessFailsOnUnconfirmedOwnership(t *testing.T) {
	process := exec.Command("sleep", "30")
	require.NoError(t, process.Start())
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})

	pid := process.Process.Pid
	m := &manager{}
	require.Error(t, m.forceKillHypervisorProcess(context.Background(), &Instance{
		StoredMetadata: StoredMetadata{Id: "kill-test", HypervisorPID: &pid, SocketPath: filepath.Join(t.TempDir(), "missing.sock")},
	}))
	assert.NoError(t, syscall.Kill(pid, 0), "process with unconfirmed socket ownership must not be killed")
}

func TestRefreshHypervisorPIDPrefersSocketOwnerOverLiveStoredPID(t *testing.T) {
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
	stored := StoredMetadata{HypervisorPID: &stalePID, SocketPath: socketPath}
	refreshHypervisorPID(&stored, StateRunning)
	require.NotNil(t, stored.HypervisorPID)
	assert.Equal(t, owner.Process.Pid, *stored.HypervisorPID)
}

func TestVGPUAssignmentClaimedByLiveInstanceProtectsReusedPIDClaim(t *testing.T) {
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

	m := &manager{paths: paths.New(t.TempDir())}
	const devicePath = "/sys/bus/pci/devices/0000:82:00.4"
	stalePID := stale.Process.Pid
	require.NoError(t, m.ensureDirectories("live-claimant"))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:            "live-claimant",
		GPUFramework:  devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath: devicePath,
		HypervisorPID: &stalePID,
		SocketPath:    socketPath,
	}}))

	claimed, err := m.vgpuAssignmentClaimedByLiveInstance(context.Background(), "other-instance", devicePath)
	require.NoError(t, err)
	assert.True(t, claimed)
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
		StoredMetadata: StoredMetadata{Id: "kill-test", HypervisorPID: &pid, SocketPath: socketPath},
	}))
}

func TestKillHypervisorFailsOnReusedPIDWhenSocketIsGone(t *testing.T) {
	stale := exec.Command("sleep", "30")
	require.NoError(t, stale.Start())
	t.Cleanup(func() {
		_ = stale.Process.Kill()
		_ = stale.Wait()
	})

	stalePID := stale.Process.Pid
	socketPath := filepath.Join(t.TempDir(), "missing.sock")
	m := &manager{}
	require.Error(t, m.killHypervisor(context.Background(), &Instance{
		StoredMetadata: StoredMetadata{Id: "kill-test", HypervisorPID: &stalePID, SocketPath: socketPath},
	}), "unconfirmed ownership of a live stored PID must fail the kill")

	assert.NoError(t, syscall.Kill(stalePID, 0), "process with unconfirmed socket ownership must not be killed")
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
		StoredMetadata: StoredMetadata{Id: "kill-test", HypervisorPID: &matchPID, SocketPath: socketPath},
	}), "a command-line match must not satisfy destructive ownership verification")

	assert.NoError(t, syscall.Kill(matchPID, 0), "process matched only by command line must not be killed")
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
