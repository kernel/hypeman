package qemu

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetVersion_Integration is an integration test that verifies GetVersion
// works correctly with the actual QEMU binary installed on the system.
func TestGetVersion_Integration(t *testing.T) {
	// Skip if QEMU is not installed
	binaryName, err := qemuBinaryName()
	if err != nil {
		t.Skipf("Skipping test: %v", err)
	}

	_, err = exec.LookPath(binaryName)
	if err != nil {
		t.Skipf("Skipping test: QEMU binary %s not found in PATH", binaryName)
	}

	// Create starter and get version
	starter := NewStarter()
	tmpDir := t.TempDir()
	p := paths.New(tmpDir)

	version, err := starter.GetVersion(p)
	if err != nil {
		t.Skipf("Skipping test: QEMU binary is not usable: %v", err)
	}

	// Verify version is not empty
	assert.NotEmpty(t, version, "Version should not be empty")

	// Verify version matches expected format (e.g., "8.2.0", "9.0", "7.2.1")
	versionPattern := regexp.MustCompile(`^\d+\.\d+(\.\d+)?$`)
	assert.Regexp(t, versionPattern, version, "Version should match pattern X.Y or X.Y.Z")

	t.Logf("Detected QEMU version: %s", version)
}

// TestGetVersion_ParsesVersionCorrectly tests the version parsing logic
// with various version string formats.
func TestGetVersion_ParsesVersionCorrectly(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		expected string
		wantErr  bool
	}{
		{
			name:     "debian format",
			output:   "QEMU emulator version 8.2.0 (Debian 1:8.2.0+dfsg-1)",
			expected: "8.2.0",
		},
		{
			name:     "simple format",
			output:   "QEMU emulator version 9.0.0",
			expected: "9.0.0",
		},
		{
			name:     "two part version",
			output:   "QEMU emulator version 9.0",
			expected: "9.0",
		},
		{
			name:     "with git info",
			output:   "QEMU emulator version 7.2.1 (qemu-7.2.1-1.fc38)",
			expected: "7.2.1",
		},
		{
			name:    "invalid format",
			output:  "Some random output",
			wantErr: true,
		},
		{
			name:    "empty output",
			output:  "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use the same regex as in GetVersion
			re := regexp.MustCompile(`version (\d+\.\d+(?:\.\d+)?)`)
			matches := re.FindStringSubmatch(tt.output)

			if tt.wantErr {
				assert.Less(t, len(matches), 2, "Should not match for invalid input")
			} else {
				require.GreaterOrEqual(t, len(matches), 2, "Should find version match")
				assert.Equal(t, tt.expected, matches[1], "Parsed version should match expected")
			}
		})
	}
}

// probeRetryingETXTBSY runs versionFromBinary against a just-written script
// with a fresh timeout-bounded context per attempt, retrying when exec fails
// with ETXTBSY: another parallel test's fork can transiently inherit the
// script's write descriptor between its own fork and exec (the well-known
// fork/exec text-file-busy race), and a retry must not consume the probe
// deadline the caller asserts on. Returns the final attempt's start time.
func probeRetryingETXTBSY(t *testing.T, binaryPath string, timeout time.Duration) (time.Time, error) {
	t.Helper()
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		start := time.Now()
		_, err := versionFromBinary(ctx, binaryPath)
		cancel()
		if errors.Is(err, syscall.ETXTBSY) && attempt < 100 {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		return start, err
	}
}

// TestVersionFromBinaryKillsProcessGroupOnTimeout pins that a cancelled
// version probe terminates its entire process tree, not just the direct
// child. A QEMU wrapper script that spawns a descendant (`sleep 60 & wait`)
// must leave neither the wrapper nor the descendant behind after the probe's
// context deadline; otherwise every capability request against a wedged
// binary would orphan a subprocess under PID 1.
func TestVersionFromBinaryKillsProcessGroupOnTimeout(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	parentPIDFile := filepath.Join(dir, "parent.pid")
	childPIDFile := filepath.Join(dir, "child.pid")
	binaryPath := filepath.Join(dir, "qemu-system-fake")
	// Write both PIDs before blocking so the test can always observe them,
	// then hang in `wait` with a background descendant — the shape that
	// leaks when cancellation only signals the direct child.
	script := "#!/bin/sh\n" +
		"echo $$ > " + parentPIDFile + "\n" +
		"sleep 60 &\n" +
		"echo $! > " + childPIDFile + "\n" +
		"wait\n"
	require.NoError(t, os.WriteFile(binaryPath, []byte(script), 0o755))

	start, err := probeRetryingETXTBSY(t, binaryPath, 250*time.Millisecond)
	require.Error(t, err, "a hung probe must fail at the deadline")
	require.Less(t, time.Since(start), 10*time.Second,
		"a hung probe must return at the deadline, not run to completion")

	readPID := func(path string) int {
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr, "probe script must have recorded %s before hanging", path)
		pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
		require.NoError(t, convErr)
		return pid
	}
	parentPID := readPID(parentPIDFile)
	childPID := readPID(childPIDFile)

	processGone := func(pid int) bool {
		// Signal 0 probes existence; a killed-but-unreaped descendant still
		// answers until init reaps it, hence Eventually below.
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}
	require.Eventually(t, func() bool {
		return processGone(parentPID) && processGone(childPID)
	}, 5*time.Second, 20*time.Millisecond,
		"both the probe wrapper (pid %d) and its descendant (pid %d) must be killed with the process group",
		parentPID, childPID)
}

// TestVersionFromBinaryKillsDescendantsWhenWrapperExitsFirst pins the
// completion-path group kill. cmd.Cancel only fires on context cancellation,
// so when the direct wrapper prints a valid version and exits immediately
// while a background descendant keeps the inherited stdout pipe open, no
// cancellation ever runs: Output merely unblocks after WaitDelay with
// ErrWaitDelay and — before the fix — the descendant survived, so every
// capability-cache refresh against such a binary leaked one process.
func TestVersionFromBinaryKillsDescendantsWhenWrapperExitsFirst(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	childPIDFile := filepath.Join(dir, "child.pid")
	binaryPath := filepath.Join(dir, "qemu-system-fake")
	// The wrapper answers the probe correctly, spawns a descendant that
	// inherits (and holds) the stdout pipe, and exits before the context
	// deadline — the shape where cmd.Cancel never runs.
	script := "#!/bin/sh\n" +
		"echo 'QEMU emulator version 8.2.0'\n" +
		"sleep 60 &\n" +
		"echo $! > " + childPIDFile + "\n" +
		"exit 0\n"
	require.NoError(t, os.WriteFile(binaryPath, []byte(script), 0o755))

	start, err := probeRetryingETXTBSY(t, binaryPath, versionProbeTimeout)
	// The descendant holds the output pipe past WaitDelay, so the probe
	// reports ErrWaitDelay rather than success; what must never happen is
	// the descendant outliving the probe.
	require.ErrorIs(t, err, exec.ErrWaitDelay,
		"a wrapper whose descendant holds the output pipe completes via WaitDelay")
	require.Less(t, time.Since(start), versionProbeTimeout,
		"the wrapper exited immediately; the probe must return at WaitDelay, not the context deadline")

	// The wrapper wrote the descendant's pid before exiting, and the probe
	// cannot return before the wrapper exits, so the file is complete here.
	data, readErr := os.ReadFile(childPIDFile)
	require.NoError(t, readErr, "probe wrapper must have recorded its descendant's pid before exiting")
	childPID, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, convErr)

	require.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(childPID, 0), syscall.ESRCH)
	}, 5*time.Second, 20*time.Millisecond,
		"background descendant (pid %d) must be killed when the probe completes, even though the wrapper exited before the context deadline",
		childPID)
}

func TestSaveAndLoadVMConfigPreservesRestoreContract(t *testing.T) {
	dir := t.TempDir()
	want := savedVMConfig{
		VMConfig:    hypervisor.VMConfig{VsockCID: 3},
		MachineType: MachineTypeMicroVM,
		QEMUVersion: "8.2.0",
	}
	require.NoError(t, saveVMConfig(dir, want))
	got, err := loadVMConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestValidateRestoreVersion(t *testing.T) {
	assert.Error(t, validateRestoreVersions("", "8.2.0"))
	assert.Error(t, validateRestoreVersions("unknown", "8.2.0"))
	assert.Error(t, validateRestoreVersions("8.1.0", "8.2.0"))
	assert.NoError(t, validateRestoreVersions("8.2.0", "8.2.0"))
}

func TestShouldRetryWithReducedBalloon(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "unsupported free page reporting",
			err:  errors.New("Property 'virtio-balloon-device.free-page-reporting' not found"),
			want: true,
		},
		{
			name: "unsupported deflate option",
			err:  errors.New("Parameter 'deflate-on-oom' is unexpected"),
			want: true,
		},
		{
			name: "free-page-hint requires iothread",
			err:  errors.New("qemu-system-x86_64: -device virtio-balloon-pci,...: 'free-page-hint' requires 'iothread' to be set"),
			want: true,
		},
		{
			name: "non-balloon start error",
			err:  errors.New("wait for socket /tmp/qemu.sock: timed out after 10s"),
			want: false,
		},
		{
			name: "transient monitor connection refused",
			err:  errors.New("create client: create qemu client: create socket monitor: dial unix /tmp/qemu.sock: connect: connection refused"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shouldRetryWithReducedBalloon(tt.err))
		})
	}
}

func TestShouldRetrySameConfig(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "monitor connection refused",
			err:  errors.New("create client: dial unix /tmp/qemu.sock: connect: connection refused"),
			want: true,
		},
		{
			name: "socket race no such file",
			err:  errors.New("create socket monitor: dial unix /tmp/qemu.sock: connect: no such file or directory"),
			want: true,
		},
		{
			name: "timeout",
			err:  errors.New("wait for socket /tmp/qemu.sock: timed out after 10s"),
			want: true,
		},
		{
			name: "explicit balloon incompatibility should not use same-config retry",
			err:  errors.New("vmm.log: Property 'virtio-balloon-device.free-page-reporting' not found"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shouldRetrySameConfig(tt.err))
		})
	}
}

func TestStartManagedProcessCleanupRemovesSocketAndReapsExitedProcess(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "qemu.sock")
	require.NoError(t, os.WriteFile(socketPath, []byte("stale"), 0600))

	cmd := exec.Command("sh", "-c", "exit 0")
	proc, err := startManagedProcess(cmd, socketPath)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, exited := proc.checkExited()
		return exited
	}, time.Second, 10*time.Millisecond)

	proc.cleanup()

	require.NoFileExists(t, socketPath)
	require.NotNil(t, cmd.ProcessState)
	assert.True(t, cmd.ProcessState.Exited())
}

func TestWaitForSocketOrExitReturnsEarlyWhenProcessDies(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "qemu.sock")

	cmd := exec.Command("sh", "-c", "exit 7")
	proc, err := startManagedProcess(cmd, socketPath)
	require.NoError(t, err)

	start := time.Now()
	err = waitForSocketOrExit(socketPath, time.Second, proc)
	require.Error(t, err)
	assert.ErrorContains(t, err, "qemu exited early")
	assert.Less(t, time.Since(start), 500*time.Millisecond)
	require.NoFileExists(t, socketPath)
	require.NotNil(t, cmd.ProcessState)
	assert.True(t, cmd.ProcessState.Exited())
}
