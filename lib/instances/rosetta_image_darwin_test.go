//go:build darwin

package instances

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/system"
	"github.com/stretchr/testify/require"
)

// dumpVZGuestSerialLogs prints each guest's serial console log (where the guest
// init's messages land), to diagnose guest-side boot/setup failures.
func dumpVZGuestSerialLogs(t *testing.T, tmpDir string) {
	t.Helper()
	logs, _ := filepath.Glob(filepath.Join(tmpDir, "guests", "*", "logs", "app.log"))
	for _, f := range logs {
		if content, err := os.ReadFile(f); err == nil && len(content) > 0 {
			t.Logf("guest serial log (%s):\n%s", f, string(content))
		}
	}
}

// TestVZRosettaImageX86 is the faithful Rosetta test: it pulls a real amd64
// Docker image and boots it on the arm64 vz host. The whole guest userland is
// x86-64, and Rosetta auto-enables because the image platform (linux/amd64)
// differs from the arm64 host — there is no user-facing emulation flag. Execing
// a real on-disk amd64 binary then dispatches through Rosetta, proving emulation
// end to end.
func TestVZRosettaImageX86(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("vz tests require macOS")
	}

	mgr, tmpDir := setupVZTestManager(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	// Pull the amd64 variant of alpine:3.19. The prewarm step mirrors this ref at
	// linux/amd64 to the local registry, so it serves a plain amd64 manifest.
	ref := integrationTestImageRef(t, "docker.io/library/alpine:3.19")
	img, err := imageManager.CreateImage(ctx, images.CreateImageRequest{Name: ref, Platform: "linux/amd64"})
	require.NoError(t, err)
	for i := 0; i < 60; i++ {
		got, err := imageManager.GetImage(ctx, img.Name)
		if err == nil && got.Status == images.StatusReady {
			img = got
			break
		}
		if err == nil && got.Status == images.StatusFailed {
			t.Fatalf("image build failed: %s", *got.Error)
		}
		time.Sleep(time.Second)
	}
	require.Equal(t, images.StatusReady, img.Status, "amd64 image should be ready")
	require.Equal(t, "linux/amd64", img.Platform, "pulled image should be linux/amd64")

	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	// No EnableRosetta flag: the manager derives it from the amd64 image on the
	// arm64 host.
	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "test-vz-rosetta-image",
		Image:          ref,
		Platform:       "linux/amd64",
		Size:           2 * 1024 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          2,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeVZ,
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
		dumpVZGuestSerialLogs(t, tmpDir)
		require.NoError(t, err)
	}
	require.NotNil(t, inst)
	t.Cleanup(func() { mgr.DeleteInstance(ctx, inst.Id) })

	require.NoError(t, waitForExecAgent(ctx, mgr, inst.Id, 30*time.Second), "guest agent should be ready")

	// The payoff: exec a real amd64 ELF shipped in the image (busybox), so a
	// genuine x86-64 binary dispatches through Rosetta — not a shell builtin.
	// uname -m is unreliable under Rosetta, so assert on the command output and a
	// file the amd64 alpine rootfs is known to contain.
	out, code, err := vzExecCommand(ctx, inst, "/bin/busybox", "cat", "/etc/alpine-release")
	if err != nil || code != 0 || !strings.HasPrefix(strings.TrimSpace(out), "3.19") {
		dumpVZShimLogs(t, tmpDir)
		dumpVZGuestSerialLogs(t, tmpDir)
		t.Fatalf("amd64 image binary did not run via Rosetta: code=%d err=%v out=%q", code, err, out)
	}
}
