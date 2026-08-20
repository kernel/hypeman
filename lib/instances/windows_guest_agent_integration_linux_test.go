//go:build linux && amd64

package instances

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWindowsGuestAgentIntegration(t *testing.T) {
	if os.Getenv("HYPEMAN_RUN_WINDOWS_GUEST_CONTROL_INTEGRATION") != "1" {
		t.Skip("run by the dedicated Windows guest-control CI gate")
	}
	fixture := os.Getenv("HYPEMAN_WINDOWS_TEST_AGENT_PERSONA")
	if fixture == "" {
		fixture = "/ci/windows/persona-agent.qcow2"
	}
	if _, err := os.Stat(fixture); err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatalf("required Windows guest-agent fixture is missing: %s", fixture)
		}
		t.Skipf("Windows guest-agent fixture is unavailable: %s", fixture)
	}
	acquireHeavyIO(t)

	manager, dataDir := setupTestManagerForQEMU(t)
	p := paths.New(dataDir)
	const digestHex = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	image := &images.Image{
		Name:     "registry.example/windows/persona:guest-agent-integration",
		Digest:   "sha256:" + digestHex,
		Platform: "windows/amd64",
		Status:   images.StatusReady,
		Machine: &images.MachineImage{
			Kind:        images.MachineImageWindowsPersona,
			Base:        "registry.example/windows/base@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
			TPM:         "2.0",
			SecureBoot:  "required",
			VirtualSize: 80 << 30,
		},
	}
	manager.imageManager = windowsFixtureImageManager{image: image}
	personaPath, err := images.GetMachineDiskPath(p, image.Name, image.Digest, image.Machine)
	require.NoError(t, err)
	require.NoError(t, forkvm.CopyRegularFile(fixture, personaPath))
	require.NoError(t, os.Chmod(personaPath, 0444))

	ctx := context.Background()
	instance, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:       "windows-guest-agent-integration",
		Image:      image.Name,
		Platform:   "windows/amd64",
		Size:       8 << 30,
		Vcpus:      4,
		Hypervisor: hypervisor.TypeQEMU,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), manager, instance.Id) })

	require.Eventually(t, func() bool {
		current, err := manager.GetInstance(ctx, instance.Id)
		return err == nil && current.State == StateRunning
	}, 4*time.Minute, time.Second, "Windows guest agent did not become ready")

	dialer, err := manager.GetVsockDialer(ctx, instance.Id)
	require.NoError(t, err)

	var stdout, stderr bytes.Buffer
	jobStart := time.Now()
	exit, err := guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command: []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `Copy-Item "$env:SystemRoot\System32\ping.exe" "$env:TEMP\hypeman-job-child.exe" -Force; & "$env:TEMP\hypeman-job-child.exe" -n 60 127.0.0.1`},
		Stdout:  &stdout,
		Stderr:  &stderr,
		Timeout: 2,
	})
	require.NoError(t, err, stderr.String())
	assert.Less(t, time.Since(jobStart), 10*time.Second, "timed out process tree did not terminate promptly")

	time.Sleep(5 * time.Second)
	stdout.Reset()
	stderr.Reset()
	exit, err = guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command: []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `if (Get-Process hypeman-job-child -ErrorAction SilentlyContinue) { exit 42 }`},
		Stdout:  &stdout,
		Stderr:  &stderr,
		Timeout: 15,
	})
	require.NoError(t, err, stderr.String())
	require.Equal(t, 0, exit.Code, "job object left a child process running")

	stdout.Reset()
	stderr.Reset()
	exit, err = guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command: []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "[Console]::Out.Write('HYPEMAN_SYSTEM_OK')"},
		Stdout:  &stdout,
		Stderr:  &stderr,
		Timeout: 30,
	})
	require.NoError(t, err, stderr.String())
	require.Equal(t, 0, exit.Code)
	assert.Equal(t, "HYPEMAN_SYSTEM_OK", stdout.String())

	stdout.Reset()
	stderr.Reset()
	resizes := make(chan *guest.WindowSize, 1)
	resizes <- &guest.WindowSize{Rows: 37, Cols: 101}
	close(resizes)
	exit, err = guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command:    []string{"cmd.exe", "/d", "/c", "ping -n 2 127.0.0.1 >nul & echo HYPEMAN_CONPTY_OK"},
		Stdout:     &stdout,
		Stderr:     &stderr,
		TTY:        true,
		Rows:       31,
		Cols:       97,
		ResizeChan: resizes,
		Timeout:    30,
	})
	require.NoError(t, err, stderr.String())
	require.Equal(t, 0, exit.Code)
	assert.Contains(t, stdout.String(), "HYPEMAN_CONPTY_OK")

	stdout.Reset()
	exit, err = guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command: []string{"cmd.exe", "/d", "/c", "echo", "HYPEMAN_DESKTOP_OK"},
		Stdout:  &stdout,
		Session: guest.ExecSession_EXEC_SESSION_DESKTOP,
		Timeout: 30,
	})
	require.NoError(t, err)
	require.Equal(t, 0, exit.Code)
	assert.Contains(t, stdout.String(), "HYPEMAN_DESKTOP_OK")

	source := filepath.Join(t.TempDir(), "roundtrip.txt")
	require.NoError(t, os.WriteFile(source, []byte("HYPEMAN_COPY_OK"), 0644))
	require.NoError(t, guest.CopyToInstance(ctx, dialer, guest.CopyToInstanceOptions{
		SrcPath: source,
		DstPath: `C:\ProgramData\Hypeman\roundtrip.txt`,
	}))
	destination := t.TempDir()
	require.NoError(t, guest.CopyFromInstance(ctx, dialer, guest.CopyFromInstanceOptions{
		SrcPath: `C:\ProgramData\Hypeman\roundtrip.txt`,
		DstPath: destination,
	}))
	contents, err := os.ReadFile(filepath.Join(destination, "roundtrip.txt"))
	require.NoError(t, err)
	assert.Equal(t, "HYPEMAN_COPY_OK", string(contents))
}
