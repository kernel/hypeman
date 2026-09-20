//go:build linux

package instances

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func init() {
	if os.Getenv("HYPEMAN_TEST_EXIT_LEADER") != "1" {
		return
	}
	// init runs on the startup thread. Exit only that thread, leaving a worker
	// alive to model a zombie QEMU leader with a vhost task still closing VFIO.
	runtime.LockOSThread()
	go func() {
		fmt.Fprintln(os.Stdout, "ready")
		_, _ = os.Stdin.Read(make([]byte, 1))
		os.Exit(0)
	}()
	syscall.Syscall(syscall.SYS_EXIT, 0, 0, 0)
}

func TestProcessExitWaitsForZombieLeadersTasks(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), "HYPEMAN_TEST_EXIT_LEADER=1")
	stdin, err := child.StdinPipe()
	require.NoError(t, err)
	stdout, err := child.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, child.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	_, err = bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	pid := child.Process.Pid
	require.Eventually(t, func() bool {
		state, err := readLinuxProcessState(pid)
		return err == nil && state == "Z"
	}, 5*time.Second, 10*time.Millisecond)

	require.True(t, ProcessExists(pid), "a zombie leader does not imply its tasks have exited")
	require.False(t, WaitForProcessExit(pid, 50*time.Millisecond))
	// A sibling process gets ECHILD from wait4, as the API does after restart.
	observer := exec.Command(os.Args[0], "-test.run=^TestProcessExitObserverHelper$")
	observer.Env = append(os.Environ(), "HYPEMAN_TEST_WAIT_PID="+strconv.Itoa(pid))
	output, err := observer.CombinedOutput()
	require.NoError(t, err, "%s", output)

	require.NoError(t, stdin.Close())
	require.True(t, WaitForProcessExit(pid, 5*time.Second))
	require.False(t, ProcessExists(pid))
}

func TestProcessExitObserverHelper(t *testing.T) {
	value := os.Getenv("HYPEMAN_TEST_WAIT_PID")
	if value == "" {
		return
	}
	pid, err := strconv.Atoi(value)
	require.NoError(t, err)
	var status syscall.WaitStatus
	_, err = syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	require.ErrorIs(t, err, syscall.ECHILD)
	require.False(t, WaitForProcessExit(pid, 50*time.Millisecond))
	identity := HypervisorProcessIdentity{}
	identity.Set(pid)
	resolved, err := resolveLiveHypervisorPID(identity, "")
	require.NoError(t, err)
	require.Equal(t, pid, resolved)
	m := &manager{}
	require.True(t, m.vgpuHypervisorMayBeAlive(t.Context(), &StoredMetadata{HypervisorProcessIdentity: identity}))
}
