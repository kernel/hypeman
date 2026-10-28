//go:build darwin

package ingress

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/kernel/hypeman/lib/paths"
)

// CaddyVersion is the version of Caddy to use.
const CaddyVersion = "v2.10.2"

// ErrCaddyNotEmbedded indicates Caddy is not embedded on macOS.
var ErrCaddyNotEmbedded = fmt.Errorf("caddy binary is not embedded on macOS; build it with `make build-caddy-darwin` and install it next to hypeman or on PATH")

// ExtractCaddyBinary on macOS finds Caddy next to the hypeman binary, then on PATH.
// Unlike Linux, we don't embed the binary on macOS.
func ExtractCaddyBinary(p *paths.Paths) (string, error) {
	if exe, err := os.Executable(); err == nil {
		path := filepath.Join(filepath.Dir(exe), "caddy")
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return path, nil
		}
	}
	path, err := exec.LookPath("caddy")
	if err != nil {
		return "", ErrCaddyNotEmbedded
	}
	return path, nil
}

// GetCaddyBinaryPath returns path to Caddy on macOS.
func GetCaddyBinaryPath(p *paths.Paths) (string, error) {
	return ExtractCaddyBinary(p)
}
