package instances

import (
	"os"
	"testing"

	"github.com/kernel/hypeman/lib/network"
	"github.com/nrednav/cuid2"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

func TestRecoveryRemovesKernelTAPWithoutAllocation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("creating a TAP requires CAP_NET_ADMIN")
	}
	if _, err := os.Stat("/dev/net/tun"); os.IsNotExist(err) {
		t.Skip("no TUN device")
	}
	m, _ := setupTestManager(t)
	id := cuid2.Generate()
	createPendingForkWithoutAllocation(t, m, id)
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{Name: network.GenerateTAPName(id)},
		Mode:      netlink.TUNTAP_MODE_TAP,
	}
	require.NoError(t, netlink.LinkAdd(tap))
	t.Cleanup(func() { _ = netlink.LinkDel(tap) })
	// A second guest can share the short TAP name. Leave it alone until
	// the full instance ID unambiguously identifies it.
	other := m.paths.InstanceDir(id + "-other")
	require.NoError(t, os.Mkdir(other, 0755))
	m.RecoverInterruptedForks(t.Context())
	_, err := netlink.LinkByName(tap.Name)
	require.NoError(t, err)
	require.FileExists(t, m.paths.InstanceMetadata(id))
	require.NoError(t, os.Remove(other))

	m.RecoverInterruptedForks(t.Context())

	var notFound netlink.LinkNotFoundError
	_, err = netlink.LinkByName(tap.Name)
	require.ErrorAs(t, err, &notFound)
	require.NoDirExists(t, m.paths.InstanceDir(id))
}
