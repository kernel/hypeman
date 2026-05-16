//go:build linux

package instances

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstanceIDFromRuntimeCmdline(t *testing.T) {
	t.Parallel()

	require.Equal(t,
		"abc123",
		instanceIDFromRuntimeCmdline("/var/lib/hypeman/guests", "/usr/bin/qemu-system-x86_64 -chardev socket,path=/var/lib/hypeman/guests/abc123/qemu.sock"),
	)
	require.Equal(t,
		"fc456",
		instanceIDFromRuntimeCmdline("/var/lib/hypeman/guests", "/var/lib/hypeman/system/binaries/firecracker --api-sock /var/lib/hypeman/guests/fc456/fc.sock"),
	)
	require.Empty(t,
		instanceIDFromRuntimeCmdline("/var/lib/hypeman/guests", "/usr/bin/qemu-system-x86_64 -monitor /tmp/qemu.sock"),
	)
}
