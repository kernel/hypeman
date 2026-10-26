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

func TestIngressResolverRefusesMacOSGuest(t *testing.T) {
	inst := &Instance{StoredMetadata: StoredMetadata{Id: "mac", NetworkEnabled: true, IP: "192.168.64.2", MacOS: &images.MacOSImage{}}}
	r := &IngressResolver{manager: fakeMinPrefixManager{inst: inst}}
	_, err := r.ResolveInstanceIP(context.Background(), "mac")
	require.Error(t, err)
	require.Contains(t, err.Error(), "macOS guest")
}
