package hypervisor

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRegisterCapabilitiesCompat pins the deprecated RegisterCapabilities
// wrapper that keeps custom backends built against earlier versions of this
// module compiling: it must register a runtime whose capabilities resolve to
// exactly the given static set and which reports available (no launch
// check), matching the old registration semantics.
func TestRegisterCapabilitiesCompat(t *testing.T) {
	t.Parallel()
	typ := Type("register-capabilities-compat-test")
	want := Capabilities{SupportsSnapshot: true, SupportsPause: true, SupportsVsock: true}
	RegisterCapabilities(typ, want)

	got, ok := CapabilitiesForType(typ)
	require.True(t, ok, "RegisterCapabilities must register the type")
	require.Equal(t, want, got)

	for _, rt := range RegisteredRuntimes() {
		if rt.Type != typ {
			continue
		}
		require.NoError(t, rt.LaunchErr)
		require.True(t, rt.Available(), "static registration implies launchability")
		require.Equal(t, want, rt.Capabilities)
		return
	}
	t.Fatal("RegisterCapabilities must add the runtime to the registry")
}
