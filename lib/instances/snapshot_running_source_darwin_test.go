//go:build darwin

package instances

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVZRunningSnapshotRoundTrip drives the running-source standby snapshot path
// end-to-end on a real vz guest. A standby snapshot taken from a RUNNING source
// saves the machine state and copies the guest disk inside the paused window,
// then resumes the source. Forking the snapshot and restoring it proves the
// snapshot resumes with guest state intact — coverage the other darwin tests
// don't provide (they snapshot from standby, never exercising the running-source
// save+copy path).
func TestVZRunningSnapshotRoundTrip(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "darwin" {
		t.Skip("vz tests require macOS")
	}
	if runtime.GOARCH != "arm64" {
		t.Skip("vz standby/restore requires Apple Silicon (arm64)")
	}
	if !isMacOS14OrLater(t) {
		t.Skip("vz standby/restore requires macOS 14+")
	}
	ensureMkfsExt4Available(t)

	mgr, tmpDir := setupVZTestManager(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	imageRef := integrationTestImageRef(t, "docker.io/library/alpine:latest")
	alpineImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{Name: imageRef})
	require.NoError(t, err)
	alpineRef, err := images.ParseNormalizedRef(alpineImage.Name)
	require.NoError(t, err)
	waitName := alpineImage.Name
	if alpineImage.Digest != "" {
		waitName = alpineRef.Repository() + "@" + alpineImage.Digest
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	require.NoError(t, imageManager.WaitForReady(waitCtx, waitName), "image should become ready")

	require.NoError(t, system.NewManager(p).EnsureSystemFiles(ctx))

	source, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "vz-running-snap-src",
		Image:          imageRef,
		Size:           2 * 1024 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeVZ,
		Cmd:            []string{"sleep", "infinity"},
	})
	if err != nil {
		dumpVZShimLogs(t, tmpDir)
		require.NoError(t, err)
	}
	sourceID := source.Id
	t.Cleanup(func() { _ = mgr.DeleteInstance(context.Background(), sourceID) })

	require.NoError(t, waitForExecAgent(ctx, mgr, sourceID, 60*time.Second), "guest agent should be ready")
	source, err = waitForInstanceState(ctx, mgr, sourceID, StateRunning, 30*time.Second)
	require.NoError(t, err)

	// Write a marker into the guest so the snapshot round-trip can be verified
	// against concrete guest state.
	const marker = "running-snap-marker"
	_, code, err := vzExecCommand(ctx, source, "sh", "-c", fmt.Sprintf("echo %s > /root/marker && sync", marker))
	require.NoError(t, err)
	require.Equal(t, 0, code, "writing the marker should succeed")

	// Standby snapshot from the RUNNING source: saves machine state and copies the
	// guest disk inside the paused window, then resumes the source.
	snapshot, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: "running-snap",
	})
	if err != nil {
		dumpVZShimLogs(t, tmpDir)
		require.NoError(t, err)
	}
	require.Equal(t, SnapshotKindStandby, snapshot.Kind)

	// The save must have written the raw machine state into the snapshot payload.
	stateDir := filepath.Join(p.SnapshotGuestDir(snapshot.Id), "snapshots", "snapshot-latest")
	assert.FileExists(t, filepath.Join(stateDir, "machine-state.vzm"), "snapshot should contain the saved machine state")

	// The running source is resumed after the snapshot; it must remain reachable
	// with its guest state intact.
	_, err = waitForInstanceState(ctx, mgr, sourceID, StateRunning, 60*time.Second)
	require.NoError(t, err, "source should be resumed to running after the snapshot")
	require.NoError(t, waitForExecAgent(ctx, mgr, sourceID, 60*time.Second))
	srcInst, err := mgr.GetInstance(ctx, sourceID)
	require.NoError(t, err)
	out, code, err := vzExecCommand(ctx, srcInst, "cat", "/root/marker")
	require.NoError(t, err)
	require.Equal(t, 0, code)
	assert.Equal(t, marker, strings.TrimSpace(out), "source guest state intact after the running-source snapshot")

	require.NoError(t, mgr.DeleteInstance(ctx, sourceID))

	// Fork the snapshot into a standby instance, then restore it. A successful
	// resume proves the running-source snapshot produced a restorable snapshot.
	forked, err := mgr.ForkSnapshot(ctx, snapshot.Id, ForkSnapshotRequest{Name: "running-snap-fork", TargetState: StateStandby})
	if err != nil {
		dumpVZShimLogs(t, tmpDir)
		require.NoError(t, err)
	}
	forkID := forked.Id
	t.Cleanup(func() { _ = mgr.DeleteInstance(context.Background(), forkID) })

	restored, err := mgr.RestoreInstance(ctx, forkID)
	if err != nil {
		dumpVZShimLogs(t, tmpDir)
		require.NoError(t, err)
	}
	assert.Contains(t, []State{StateInitializing, StateRunning}, restored.State)

	require.NoError(t, waitForExecAgent(ctx, mgr, forkID, 60*time.Second), "restored fork guest agent should be ready")
	forkInst, err := mgr.GetInstance(ctx, forkID)
	require.NoError(t, err)
	forkOut, code, err := vzExecCommand(ctx, forkInst, "cat", "/root/marker")
	if err != nil {
		dumpVZShimLogs(t, tmpDir)
	}
	require.NoError(t, err, "exec should succeed on the restored fork")
	require.Equal(t, 0, code)
	assert.Equal(t, marker, strings.TrimSpace(forkOut), "restored fork must carry the source's guest state from the running-source snapshot")
}
