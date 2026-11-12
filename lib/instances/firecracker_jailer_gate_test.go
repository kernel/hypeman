//go:build linux

package instances

import (
	"os"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/hypervisor/firecracker"
)

// testFirecrackerJailer is the jailer config the firecracker test managers use.
// HYPEMAN_TEST_FIRECRACKER_JAILER=1 runs the existing firecracker suite under the
// jailer, using the embedded jailer unless HYPEMAN_TEST_JAILER_BINARY names one.
// Without the flag the package behaves as before.
var testFirecrackerJailer config.FirecrackerJailerConfig

func init() {
	if os.Getenv("HYPEMAN_TEST_FIRECRACKER_JAILER") != "1" {
		return
	}
	testFirecrackerJailer = config.FirecrackerJailerConfig{
		Enabled:    true,
		BinaryPath: os.Getenv("HYPEMAN_TEST_JAILER_BINARY"),
		UID:        2001,
		GID:        2001,
	}
	firecracker.SetJailerConfig(firecracker.JailerConfig{
		Enabled:    true,
		BinaryPath: testFirecrackerJailer.BinaryPath,
		UID:        testFirecrackerJailer.UID,
		GID:        testFirecrackerJailer.GID,
	})
}
