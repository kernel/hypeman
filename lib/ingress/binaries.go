package ingress

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/onkernel/hypeman/lib/paths"
)

//go:embed binaries/caddy/v2.10.2/x86_64/caddy
//go:embed binaries/caddy/v2.10.2/aarch64/caddy
var caddyBinaryFS embed.FS

// CaddyVersion is the version of Caddy embedded in this build.
const CaddyVersion = "v2.10.2"

// ExtractCaddyBinary extracts the embedded Caddy binary to the data directory.
// Returns the path to the extracted binary.
func ExtractCaddyBinary(p *paths.Paths) (string, error) {
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x86_64"
	} else if arch == "arm64" {
		arch = "aarch64"
	}

	embeddedPath := fmt.Sprintf("binaries/caddy/%s/%s/caddy", CaddyVersion, arch)
	extractPath := p.CaddyBinary(CaddyVersion, arch)

	// Check if already extracted
	if _, err := os.Stat(extractPath); err == nil {
		return extractPath, nil
	}

	// Create directory
	if err := os.MkdirAll(filepath.Dir(extractPath), 0755); err != nil {
		return "", fmt.Errorf("create caddy binary dir: %w", err)
	}

	// Read embedded binary
	data, err := caddyBinaryFS.ReadFile(embeddedPath)
	if err != nil {
		return "", fmt.Errorf("read embedded caddy binary: %w", err)
	}

	// Write to filesystem
	if err := os.WriteFile(extractPath, data, 0755); err != nil {
		return "", fmt.Errorf("write caddy binary: %w", err)
	}

	return extractPath, nil
}

// GetCaddyBinaryPath returns path to extracted binary, extracting if needed.
func GetCaddyBinaryPath(p *paths.Paths) (string, error) {
	return ExtractCaddyBinary(p)
}
