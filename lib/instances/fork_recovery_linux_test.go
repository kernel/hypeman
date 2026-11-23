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
	for _, tc := range []struct {
		name       string
		sharedName bool
	}{
		{"own name", false},
		{"shared name", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := setupTestManager(t)
			id := cuid2.Generate()
			createPendingForkWithoutAllocation(t, m, id)
			tap := &netlink.Tuntap{
				LinkAttrs: netlink.LinkAttrs{Name: network.GenerateTAPName(id)},
				Mode:      netlink.TUNTAP_MODE_TAP,
			}
			require.NoError(t, netlink.LinkAdd(tap))
			t.Cleanup(func() { _ = netlink.LinkDel(tap) })
			if tc.sharedName {
				// A second guest can share the short TAP name, so the TAP may
				// be its own. It is left for the TAP reconciler.
				require.NoError(t, os.Mkdir(m.paths.InstanceDir(id+"-other"), 0755))
			}

			m.RecoverInterruptedForks(t.Context())
			require.NoDirExists(t, m.paths.InstanceDir(id))
			_, err := netlink.LinkByName(tap.Name)
			if tc.sharedName {
				require.NoError(t, err)
				return
			}
			var notFound netlink.LinkNotFoundError
			require.ErrorAs(t, err, &notFound)
		})
	}
}
