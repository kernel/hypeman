//go:build darwin

package instances

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rosettaProbeAMD64 is a tiny static x86-64 ELF that prints ROSETTA_X86_OK and
// exits 0 using only raw syscalls. It runs only via Rosetta's binfmt_misc
// dispatch. See testdata/README.md.
//
//go:embed testdata/rosetta_probe_amd64
var rosettaProbeAMD64 []byte

// vzExecStdin runs a command in the guest, feeding stdin from s.
func vzExecStdin(ctx context.Context, inst *Instance, s string, command ...string) (string, int, error) {
	dialer, err := hypervisor.NewVsockDialer(inst.HypervisorType, inst.VsockSocket, inst.VsockCID)
	if err != nil {
		return "", -1, err
	}
	var stdout, stderr bytes.Buffer
	exit, err := guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command: command,
		Stdin:   strings.NewReader(s),
		Stdout:  &stdout,
		Stderr:  &stderr,
		TTY:     false,
	})
	if err != nil {
		return stderr.String(), -1, err
	}
	return stdout.String(), exit.Code, nil
}

// TestVZRosettaX86Exec is the end-to-end Rosetta test. It boots a vz Linux guest
// with EnableRosetta, then writes a static x86-64 ELF into the guest and runs it.
// The binary executes only if the guest mounted the Rosetta virtio-fs share and
// registered the binfmt_misc handler that routes amd64 ELFs through Rosetta, and
// the host attached the Rosetta share (which requires Rosetta installed on the
// host). Image pulls are host-arch (arm64), so an injected amd64 binary is the
// way to exercise emulation.
func TestVZRosettaX86Exec(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("vz tests require macOS")
	}

	mgr, tmpDir := setupVZTestManager(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	ref := integrationTestImageRef(t, "docker.io/library/alpine:latest")
	img, err := imageManager.CreateImage(ctx, images.CreateImageRequest{Name: ref})
	require.NoError(t, err)
	for i := 0; i < 60; i++ {
		got, err := imageManager.GetImage(ctx, img.Name)
		if err == nil && got.Status == images.StatusReady {
			break
		}
		if err == nil && got.Status == images.StatusFailed {
			t.Fatalf("image build failed: %s", *got.Error)
		}
		time.Sleep(time.Second)
	}

	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "test-vz-rosetta",
		Image:          ref,
		Size:           2 * 1024 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          2,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeVZ,
		EnableRosetta:  true,
		Cmd:            []string{"sleep", "infinity"},
	})
	if err != nil {
		// The host can only attach the Rosetta share when Rosetta is installed
		// (softwareupdate --install-rosetta). Skip rather than fail where it is
		// unavailable, so this test runs only where emulation can actually work.
		if msg := err.Error(); strings.Contains(msg, "not installed") || strings.Contains(msg, "not supported") {
			t.Skipf("host cannot attach Rosetta share: %v", err)
		}
		dumpVZShimLogs(t, tmpDir)
		require.NoError(t, err)
	}
	require.NotNil(t, inst)
	t.Cleanup(func() { mgr.DeleteInstance(ctx, inst.Id) })

	require.NoError(t, waitForExecAgent(ctx, mgr, inst.Id, 30*time.Second), "guest agent should be ready")

	// The guest init should have registered the rosetta binfmt_misc handler.
	out, code, err := vzExecCommand(ctx, inst, "cat", "/proc/sys/fs/binfmt_misc/rosetta")
	require.NoError(t, err)
	require.Equalf(t, 0, code, "rosetta binfmt handler should be registered; got: %q", out)
	assert.Contains(t, out, "enabled")

	// Write the x86-64 probe into the guest (stdin avoids arg-length limits).
	_, code, err = vzExecStdin(ctx, inst, base64.StdEncoding.EncodeToString(rosettaProbeAMD64),
		"sh", "-c", "base64 -d > /tmp/probe && chmod +x /tmp/probe")
	require.NoError(t, err)
	require.Equal(t, 0, code, "writing the probe into the guest should succeed")

	// The payoff: an x86-64 ELF runs only if Rosetta emulation is working.
	out, code, err = vzExecCommand(ctx, inst, "/tmp/probe")
	require.NoError(t, err, "x86-64 binary should execute via Rosetta")
	require.Equalf(t, 0, code, "x86-64 probe should exit 0; output=%q", out)
	assert.Equal(t, "ROSETTA_X86_OK", strings.TrimSpace(out))
}
