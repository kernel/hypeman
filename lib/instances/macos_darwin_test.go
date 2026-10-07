//go:build darwin

package instances

import (
	"github.com/stretchr/testify/require"
	"net"
	"testing"
)

func TestMacOSLeaseNormalization(t *testing.T) {
	mac, err := net.ParseMAC("02:00:01:0a:0b:ff")
	require.NoError(t, err)
	data := "{\n ip_address=192.168.64.7\n hw_address=1,2:0:1:a:b:ff\n}\n"
	require.Equal(t, "192.168.64.7", macOSLeaseIP(data, mac))
	require.Empty(t, macOSLeaseIP("garbage", mac))
}
