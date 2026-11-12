//go:build linux

package instances

import (
	"os"

	"github.com/kernel/hypeman/lib/hypervisor/firecracker"
	"github.com/kernel/hypeman/lib/network"
)

// Runs the existing firecracker suite under the jailer when
// HYPEMAN_TEST_FIRECRACKER_JAILER=1 and HYPEMAN_TEST_JAILER_BINARY is set.
// Without them the package behaves as before.
func init() {
	bin := os.Getenv("HYPEMAN_TEST_JAILER_BINARY")
	if os.Getenv("HYPEMAN_TEST_FIRECRACKER_JAILER") != "1" || bin == "" {
		return
	}
	firecracker.SetJailerConfig(firecracker.JailerConfig{Enabled: true, BinaryPath: bin, UID: 2001, GID: 2001})
	// Production sets the TAP owner in ProvideNetworkManager; the test harness builds its own manager.
	network.SetTAPOwner(2001, 2001)
}
