package instances

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestClearMacOSStopReceiptPersistsBeforeBoot(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	require.NoError(t, m.ensureDirectories("mac"))
	stored := StoredMetadata{Id: "mac", DataDir: p.InstanceDir("mac"), MacOS: &images.MacOSImage{}, StopGraceful: true}
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: stored}))

	require.NoError(t, m.clearMacOSStopReceipt(&stored))
	require.False(t, stored.StopGraceful)
	meta, err := m.loadMetadata("mac")
	require.NoError(t, err)
	require.False(t, meta.StopGraceful, "the cleared receipt must be on disk before the guest boots")
}

func TestAssignMacOSCloneMACAvoidsStoredMACs(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	const taken = "02:00:00:00:00:01"
	require.NoError(t, m.ensureDirectories("other"))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{Id: "other", DataDir: p.InstanceDir("other"), MAC: taken}}))

	source := &images.MacOSImage{MAC: taken}
	clone := StoredMetadata{Id: "clone", NetworkEnabled: true, MAC: taken, MacOS: source}
	require.NoError(t, m.assignMacOSCloneMAC(&clone))
	require.NotEqual(t, taken, clone.MAC)
	require.Equal(t, clone.MAC, clone.MacOS.MAC)
	require.Equal(t, taken, source.MAC, "the clone must not alias its source's macOS struct")

	offline := StoredMetadata{Id: "clone2", MacOS: &images.MacOSImage{}}
	require.NoError(t, m.assignMacOSCloneMAC(&offline))
	require.Empty(t, offline.MAC, "a clone without networking gets no MAC")
}

func TestRejectedMacOSStartKeepsStopReceipt(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	require.NoError(t, m.ensureDirectories("mac"))
	stored := StoredMetadata{
		Id:             "mac",
		DataDir:        p.InstanceDir("mac"),
		HypervisorType: hypervisor.TypeVZ,
		SocketPath:     filepath.Join(t.TempDir(), strings.Repeat("s", maxMacOSSocketPath)),
		MacOS:          &images.MacOSImage{},
		StopGraceful:   true,
	}
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: stored}))

	err := m.startAndBootVM(context.Background(), &stored, nil, nil, false)
	require.ErrorIs(t, err, ErrInvalidRequest)
	meta, err := m.loadMetadata("mac")
	require.NoError(t, err)
	require.True(t, meta.StopGraceful, "a start rejected at boot admission must keep the stop receipt")
}
