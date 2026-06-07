//go:build darwin

package instances

import (
	"context"
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

// TestVZRosettaImageX86 is the faithful Rosetta test: it pulls a real amd64
// Docker image and boots it on the arm64 vz host with EnableRosetta, so the
// entire guest userland is x86-64. The amd64 entrypoint (sleep infinity) only
// runs if the host attached the Rosetta share and the guest registered the
// binfmt_misc handler; execing any image binary then proves emulation end to
// end. Unlike TestVZRosettaX86Exec, nothing is injected — the image itself is
// amd64.
func TestVZRosettaImageX86(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("vz tests require macOS")
	}

	mgr, tmpDir := setupVZTestManager(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	// Pull the amd64 variant of alpine:3.19. The prewarm step mirrors this ref
	// at amd64, so the local registry serves a plain amd64 manifest.
	ref := integrationTestImageRef(t, "docker.io/library/alpine:3.19")
	img, err := imageManager.CreateImage(ctx, images.CreateImageRequest{Name: ref, Architecture: "amd64"})
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
	require.Equal(t, "amd64", img.Architecture, "pulled image should be amd64")

	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "test-vz-rosetta-image",
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
		dumpVZGuestSerialLogs(t, tmpDir)
		require.NoError(t, err)
	}
	require.NotNil(t, inst)
	t.Cleanup(func() { mgr.DeleteInstance(ctx, inst.Id) })

	require.NoError(t, waitForExecAgent(ctx, mgr, inst.Id, 30*time.Second), "guest agent should be ready")

	// The payoff: the whole userland is amd64, so any successful image-binary
	// exec proves Rosetta. uname -m is unreliable under Rosetta, so assert on a
	// command's output instead.
	out, code, err := vzExecCommand(ctx, inst, "echo", "x86-image-ok")
	if err != nil || code != 0 || strings.TrimSpace(out) != "x86-image-ok" {
		dumpVZShimLogs(t, tmpDir)
		dumpVZGuestSerialLogs(t, tmpDir)
		t.Fatalf("amd64 image did not run via Rosetta: code=%d err=%v out=%q", code, err, out)
	}
}
