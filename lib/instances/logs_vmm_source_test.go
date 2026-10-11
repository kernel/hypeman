package instances

import (
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestVMMLogSourceFollowsMacOSShimLog(t *testing.T) {
	m := &manager{paths: paths.New(t.TempDir())}
	mac := &metadata{StoredMetadata: StoredMetadata{Id: "i1", HypervisorType: hypervisor.TypeVZ, MacOS: &images.MacOSImage{}}}
	require.Equal(t, m.paths.InstanceVZShimLog("i1"), m.vmmLogPath(mac))
	linux := &metadata{StoredMetadata: StoredMetadata{Id: "i1", HypervisorType: hypervisor.TypeVZ}}
	require.Equal(t, m.paths.InstanceVMMLog("i1"), m.vmmLogPath(linux))
}
