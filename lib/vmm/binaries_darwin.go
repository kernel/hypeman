//go:build darwin

package vmm

import (
	"fmt"

	"github.com/kernel/hypeman/lib/paths"
)

// CHVersion represents Cloud Hypervisor version
type CHVersion string

const (
	V48_0 CHVersion = "v48.0"
	V49_0 CHVersion = "v49.0"
)

// SupportedVersions lists supported Cloud Hypervisor versions.
// On macOS, Cloud Hypervisor is not supported (use vz instead).
var SupportedVersions = []CHVersion{}

// ErrNotSupportedOnMacOS indicates Cloud Hypervisor is not available on macOS
var ErrNotSupportedOnMacOS = fmt.Errorf("cloud-hypervisor is not supported on macOS; use vz hypervisor instead")

// ExtractBinary is not supported on macOS
func ExtractBinary(p *paths.Paths, version CHVersion) (string, error) {
	return "", ErrNotSupportedOnMacOS
}

// GetBinaryPath is not supported on macOS
func GetBinaryPath(p *paths.Paths, version CHVersion) (string, error) {
	return "", ErrNotSupportedOnMacOS
}
