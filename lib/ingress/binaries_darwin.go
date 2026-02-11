//go:build darwin

package ingress

import (
	"fmt"
	"os/exec"

	"github.com/kernel/hypeman/lib/paths"
)

// CaddyVersion is the version of Caddy to use.
const CaddyVersion = "v2.10.2"

// ErrCaddyNotEmbedded indicates Caddy is not embedded on macOS.
// Users should install Caddy via Homebrew or download from caddyserver.com.
var ErrCaddyNotEmbedded = fmt.Errorf("caddy binary is not embedded on macOS; install via: brew install caddy")

// ExtractCaddyBinary on macOS attempts to find Caddy in PATH.
// Unlike Linux, we don't embed the binary on macOS.
func ExtractCaddyBinary(p *paths.Paths) (string, error) {
	// Try to find caddy in PATH
	path, err := exec.LookPath("caddy")
	if err != nil {
		return "", ErrCaddyNotEmbedded
	}
	return path, nil
}

// GetCaddyBinaryPath returns path to Caddy, looking in PATH on macOS.
func GetCaddyBinaryPath(p *paths.Paths) (string, error) {
	return ExtractCaddyBinary(p)
}
