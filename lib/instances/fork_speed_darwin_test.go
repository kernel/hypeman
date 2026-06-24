//go:build darwin

package instances

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/system"
	"github.com/stretchr/testify/require"
)

// TestVZForkSpeed measures end-to-end VM fork latency on macOS: the time for
// mgr.ForkInstance to copy the guest disk, rewrite the snapshot manifest, and
// materialize a new (stopped) instance — comparing the APFS clonefile fast path
// against the sparse-copy fallback (the pre-clonefile behavior). The source is
// given a dense overlay so the disk copy, which is the part clonefile changes,
// is representative of a real fork.
//
// Gated behind HYPEMAN_FORK_BENCH=1 because it boots a VM and writes a multi-GiB
// disk; it is not part of the normal suite. Run with:
//
//	HYPEMAN_FORK_BENCH=1 go test -run TestVZForkSpeed -v -timeout=20m ./lib/instances/
func TestVZForkSpeed(t *testing.T) {
	if os.Getenv("HYPEMAN_FORK_BENCH") == "" {
		t.Skip("set HYPEMAN_FORK_BENCH=1 to run the fork-speed measurement")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("vz fork requires macOS on Apple Silicon")
	}
	if !isMacOS14OrLater(t) {
		t.Skip("vz fork requires macOS 14+")
	}
	ensureMkfsExt4Available(t)

	fillMiB := int64(2048)
	if v := os.Getenv("HYPEMAN_FORK_BENCH_MIB"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		require.NoError(t, err)
		fillMiB = n
	}
	const iters = 3

	mgr, tmpDir := setupVZTestManager(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	ref := integrationTestImageRef(t, "docker.io/library/alpine:latest")
	img, err := imageManager.CreateImage(ctx, images.CreateImageRequest{Name: ref})
	require.NoError(t, err)
	waitName := img.Name
	if img.Digest != "" {
		parsed, perr := images.ParseNormalizedRef(img.Name)
		require.NoError(t, perr)
		waitName = parsed.Repository() + "@" + img.Digest
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	require.NoError(t, imageManager.WaitForReady(waitCtx, waitName))

	require.NoError(t, system.NewManager(p).EnsureSystemFiles(ctx))

	source, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fork-bench-src",
		Image:          ref,
		Size:           2 * 1024 * 1024 * 1024,
		OverlaySize:    16 * 1024 * 1024 * 1024,
		Vcpus:          2,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeVZ,
		Cmd:            []string{"sleep", "infinity"},
	})
	if err != nil {
		dumpVZShimLogs(t, tmpDir)
		require.NoError(t, err)
	}
	t.Cleanup(func() { _ = mgr.DeleteInstance(context.Background(), source.Id) })
	require.NoError(t, waitForExecAgent(ctx, mgr, source.Id, 60*time.Second))

	// Fill the overlay with dense (random) data so the fork's disk copy has real
	// work to do — a fresh overlay is sparse and copies fast either way.
	t.Logf("writing %d MiB into the source overlay...", fillMiB)
	_, code, err := vzExecCommand(ctx, source, "sh", "-c",
		fmt.Sprintf("dd if=/dev/urandom of=/root/fill bs=1048576 count=%d 2>/dev/null && sync", fillMiB))
	require.NoError(t, err)
	require.Equalf(t, 0, code, "dd to fill overlay should succeed")

	// Stop the source so each fork copies a stable on-disk state without booting.
	_, err = mgr.StopInstance(ctx, source.Id, nil)
	require.NoError(t, err)

	// Reset the global reflink flag however the test exits — a require failure
	// mid-loop would otherwise leak the disabled state into other tests.
	t.Cleanup(func() { forkvm.SetReflinkDisabled(false) })

	for _, mode := range []struct {
		name     string
		disabled bool
	}{
		{"clonefile", false}, // with the change
		{"sparse", true},     // without the change (reflink fallback)
	} {
		forkvm.SetReflinkDisabled(mode.disabled)
		var total time.Duration
		for i := 0; i < iters; i++ {
			start := time.Now()
			forked, ferr := mgr.ForkInstance(ctx, source.Id, ForkInstanceRequest{
				Name:        fmt.Sprintf("fork-%s-%d", mode.name, i),
				TargetState: StateStopped,
			})
			elapsed := time.Since(start)
			if ferr != nil {
				dumpVZShimLogs(t, tmpDir)
				require.NoError(t, ferr)
			}
			total += elapsed
			require.NoError(t, mgr.DeleteInstance(ctx, forked.Id))
		}
		t.Logf("FORKBENCH mode=%-9s disk=%dMiB iters=%d avg=%v", mode.name, fillMiB, iters, total/iters)
	}
}
