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
	out := "  101 /tmp/vz-shim-1 -config " + config("/data/guests/a/vz.sock") + "\n" +
		"  202 /tmp/vz-shim-2 -config " + config("/data/guests/b/vz.sock") + "\n" +
		"  303 grep control_socket\n" +
		"  404 /tmp/vz-shim-3 -config " + config("/data/guests/a/vz.sock") + "\n"
	require.Equal(t, []int{101, 404}, vzShimPIDsFromPS([]byte(out), "/data/guests/a/vz.sock"))
	require.Empty(t, vzShimPIDsFromPS([]byte(out), "/data/guests/c/vz.sock"))
	require.Empty(t, vzShimPIDsFromPS([]byte(out), "/data/guests/a/vz"), "a socket path prefix does not match")
}
