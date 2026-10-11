//go:build darwin

package instances

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestMacOSBuildBundleRequiresClosedDistinctStorage(t *testing.T) {
	root := t.TempDir()
	disk := filepath.Join(root, paths.MacOSBootDiskFile)
	aux := filepath.Join(root, "mac-aux.img")
	require.NoError(t, os.WriteFile(disk, []byte("synthetic disk"), 0600))
	require.NoError(t, os.WriteFile(aux, []byte("synthetic aux"), 0600))
	inst := &Instance{State: StateStopped, StoredMetadata: StoredMetadata{DataDir: root, MacOS: &images.MacOSImage{HardwareModel: []byte{1}, MachineIdentifier: []byte{2}, MAC: "02:00:00:00:00:01", CPUs: 4, Memory: 8 << 30, GuestAgent: true}}}
	file, e := os.Open(disk)
	require.NoError(t, e)
	require.Error(t, writeStoppedMacOSBuildBundle(context.Background(), inst, disk, t.TempDir()))
	require.NoError(t, file.Close())
	out := t.TempDir()
	require.NoError(t, writeStoppedMacOSBuildBundle(context.Background(), inst, disk, out))
	_, e = images.ValidateMacOSBundle(out)
	require.NoError(t, e)
	for _, name := range []string{"disk.img", "aux.img", "config.json"} {
		info, e := os.Stat(filepath.Join(out, name))
		require.NoError(t, e)
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	}
	require.NoError(t, os.Remove(aux))
	require.NoError(t, os.Link(disk, aux))
	require.Error(t, writeStoppedMacOSBuildBundle(context.Background(), inst, disk, t.TempDir()))
	inst.State = StateRunning
	require.Error(t, writeStoppedMacOSBuildBundle(context.Background(), inst, disk, t.TempDir()))
}

func TestMacOSBuildStopAndExportRequireBoundedInputs(t *testing.T) {
	m := &manager{}
	_, e := m.StopMacOSBuildInstance(context.Background(), "unused")
	require.Error(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, e = m.StopMacOSBuildInstance(ctx, "unused")
	require.ErrorIs(t, e, context.Canceled)
	require.Error(t, m.ExportStoppedMacOSBuildInstance(context.Background(), "unused", t.TempDir()))
	require.Error(t, m.ExportStoppedMacOSBuildInstance(ctx, "unused", t.TempDir()))
}
