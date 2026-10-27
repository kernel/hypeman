package instances

import (
	"context"
	"testing"

	"github.com/kernel/hypeman/lib/images"
	"github.com/stretchr/testify/require"
)

type fakeMinPrefixManager struct {
	Manager
	inst *Instance
}

func (f fakeMinPrefixManager) getInstanceWithMinIDPrefix(context.Context, string, int) (*Instance, error) {
	return f.inst, nil
}

// A running macOS guest resolves to its vmnet lease, which the instance reports as its IP.
func TestIngressResolverResolvesMacOSGuestLease(t *testing.T) {
	inst := &Instance{StoredMetadata: StoredMetadata{Id: "mac", NetworkEnabled: true, IP: "192.168.64.2", MacOS: &images.MacOSImage{}}}
	r := &IngressResolver{manager: fakeMinPrefixManager{inst: inst}}
	ip, err := r.ResolveInstanceIP(context.Background(), "mac")
	require.NoError(t, err)
	require.Equal(t, "192.168.64.2", ip)

	inst.IP = ""
	_, err = r.ResolveInstanceIP(context.Background(), "mac")
	require.ErrorContains(t, err, "no IP assigned", "a guest without a current lease has no target")
}
