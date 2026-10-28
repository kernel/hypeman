package instances

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func macOSNetworkTestManager(t *testing.T, pool string, now *time.Time) *manager {
	t.Helper()
	m := &manager{paths: paths.New(t.TempDir()), macOSGuestSubnet: netip.MustParsePrefix(pool), now: func() time.Time { return *now }}
	require.NoError(t, os.MkdirAll(m.paths.GuestsDir(), 0755))
	return m
}

func saveAddressedInstance(t *testing.T, m *manager, id, ip string, macOS bool) {
	t.Helper()
	stored := StoredMetadata{Id: id, IP: ip, NetworkEnabled: true}
	if macOS {
		stored.MacOS = macOSGuestMetadata(1).MacOS
	}
	require.NoError(t, os.MkdirAll(m.paths.InstanceDir(id), 0755))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: stored}))
}

func networkedMacOSGuest(id string) *StoredMetadata {
	stored := macOSGuestMetadata(1)
	stored.Id = id
	stored.NetworkEnabled = true
	return stored
}

// A /27 pool holds four guest networks, whose guest addresses are .2, .10, .18 and .26.
func TestMacOSAddressAssignment(t *testing.T) {
	now := time.Now()
	m := macOSNetworkTestManager(t, "192.168.128.0/27", &now)
	saveAddressedInstance(t, m, "mac-a", "192.168.128.2", true)
	saveAddressedInstance(t, m, "linux-b", "192.168.128.10", false)

	c := networkedMacOSGuest("mac-c")
	require.NoError(t, m.assignMacOSAddress(c))
	require.Equal(t, "192.168.128.18", c.IP, "addresses of every stored instance are skipped")

	d := networkedMacOSGuest("mac-d")
	require.NoError(t, m.assignMacOSAddress(d))
	require.Equal(t, "192.168.128.26", d.IP, "a create in progress keeps its address before it is saved")

	require.ErrorIs(t, m.assignMacOSAddress(networkedMacOSGuest("mac-e")), ErrInsufficientResources)

	require.NoError(t, m.assignMacOSAddress(c))
	require.Equal(t, "192.168.128.18", c.IP, "a guest keeps the address it has")
	offline := networkedMacOSGuest("mac-f")
	offline.NetworkEnabled = false
	require.NoError(t, m.assignMacOSAddress(offline))
	require.Empty(t, offline.IP)

	m.forgetMacOSAddress("mac-d")
	e := networkedMacOSGuest("mac-e")
	require.NoError(t, m.assignMacOSAddress(e))
	require.Equal(t, "192.168.128.26", e.IP, "an unfinished create's address is free again without a hold")
}

func TestMacOSAddressReuseHold(t *testing.T) {
	now := time.Now()
	m := macOSNetworkTestManager(t, "192.168.128.0/29", &now)
	a := networkedMacOSGuest("mac-a")
	require.NoError(t, m.assignMacOSAddress(a))
	require.Equal(t, "192.168.128.2", a.IP)

	require.NoError(t, m.releaseMacOSAddress("mac-a", a.IP))
	now = now.Add(time.Minute)
	require.ErrorIs(t, m.assignMacOSAddress(networkedMacOSGuest("mac-b")), ErrInsufficientResources, "a released address is held past ingress's upstream cache")
	ok, err := m.macOSAddressAvailable("mac-c", "192.168.128.2")
	require.NoError(t, err)
	require.False(t, ok)

	now = now.Add(macOSAddressReuseHold)
	b := networkedMacOSGuest("mac-b")
	require.NoError(t, m.assignMacOSAddress(b))
	require.Equal(t, "192.168.128.2", b.IP)
}

func TestMacOSAddressAvailable(t *testing.T) {
	now := time.Now()
	m := macOSNetworkTestManager(t, "192.168.128.0/28", &now)
	saveAddressedInstance(t, m, "mac-a", "192.168.128.2", true)
	for ip, want := range map[string]bool{"192.168.128.2": false, "192.168.128.10": true, "garbage": false} {
		ok, err := m.macOSAddressAvailable("mac-b", ip)
		require.NoError(t, err)
		require.Equal(t, want, ok, ip)
	}
	ok, err := m.macOSAddressAvailable("mac-a", "192.168.128.2")
	require.NoError(t, err)
	require.True(t, ok, "an instance may keep its own address")
}

func TestMacOSGuestSubnet(t *testing.T) {
	subnet, err := macOSGuestSubnet("192.168.128.10")
	require.NoError(t, err)
	require.Equal(t, "192.168.128.8/29", subnet.String())
	_, err = macOSGuestSubnet("fd00::2")
	require.Error(t, err)
}

const deleteShutdownTestHypervisor hypervisor.Type = "macos-delete-shutdown-test"

type shutdownRecordingHypervisor struct {
	lifecycleNoopHypervisor
	calls *atomic.Int32
}

func (h shutdownRecordingHypervisor) Shutdown(context.Context) error {
	h.calls.Add(1)
	return nil
}

func (h shutdownRecordingHypervisor) Capabilities() hypervisor.Capabilities {
	return hypervisor.Capabilities{SupportsGracefulVMMShutdown: true}
}

// A killed vz-shim leaves its vmnet network behind and blocks its subnet, so delete
// asks a networked macOS guest's shim to stop the VM instead of killing it.
func TestDeleteStopsMacOSShimBeforeKilling(t *testing.T) {
	var calls atomic.Int32
	hypervisor.RegisterClientFactory(deleteShutdownTestHypervisor, func(string) (hypervisor.Hypervisor, error) {
		return shutdownRecordingHypervisor{lifecycleNoopHypervisor: lifecycleNoopHypervisor{state: hypervisor.StateRunning}, calls: &calls}, nil
	})
	m := &manager{}
	inst := func(macOS bool) *Instance {
		stored := StoredMetadata{Id: "delete-shutdown", HypervisorType: deleteShutdownTestHypervisor, NetworkEnabled: true, SocketPath: filepath.Join(t.TempDir(), "vz.sock")}
		if macOS {
			stored.MacOS = macOSGuestMetadata(1).MacOS
		}
		return &Instance{StoredMetadata: stored}
	}

	require.NoError(t, m.stopHypervisorForDelete(context.Background(), inst(true)))
	require.EqualValues(t, 1, calls.Load(), "a macOS shim is asked to stop its VM")
	require.NoError(t, m.stopHypervisorForDelete(context.Background(), inst(false)))
	require.EqualValues(t, 1, calls.Load(), "other guests are killed as before")
}
