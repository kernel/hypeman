package hypervisor

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVZShimPIDsFromPS(t *testing.T) {
	config := func(socket string) string {
		data, err := json.Marshal(map[string]string{"control_socket": socket})
		require.NoError(t, err)
		return string(data)
	}
	out := "  101 vz-shim-1 /tmp/with spaces/vz-shim-1 -config " + config("/data/guests/a/vz.sock") + "\n" +
		"  202 vz-shim-2 /tmp/vz-shim-2 -config " + config("/data/guests/b/vz.sock") + "\n" +
		"  303 editor editor -config " + config("/data/guests/a/vz.sock") + "\n" +
		"  404 vz-shim /tmp/vz-shim -config " + config("/data/guests/a/vz.sock") + "\n" +
		`  505 vz-shim-4 /tmp/vz-shim-4 -config {"nested":{"control_socket":"/data/guests/a/vz.sock"}}`
	require.Equal(t, []int{101, 404}, vzShimPIDsFromPS([]byte(out), "/data/guests/a/vz.sock"))
	require.Empty(t, vzShimPIDsFromPS([]byte(out), "/data/guests/c/vz.sock"))
	require.Empty(t, vzShimPIDsFromPS([]byte(out), "/data/guests/a/vz"), "a socket path prefix does not match")
}
