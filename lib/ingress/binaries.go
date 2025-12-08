package ingress

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/onkernel/hypeman/lib/paths"
)

// CaddyVersion is the version of Caddy embedded in this build.
const CaddyVersion = "v2.10.2"

// caddyBinaryFS and caddyArch are defined in architecture-specific files:
// - binaries_amd64.go (for x86_64)
// - binaries_arm64.go (for aarch64)

// ExtractCaddyBinary extracts the embedded Caddy binary to the data directory.
// Returns the path to the extracted binary.
func ExtractCaddyBinary(p *paths.Paths) (string, error) {
	embeddedPath := fmt.Sprintf("binaries/caddy/%s/%s/caddy", CaddyVersion, caddyArch)
	extractPath := p.CaddyBinary(CaddyVersion, caddyArch)

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
