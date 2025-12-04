package ingress

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/onkernel/hypeman/lib/paths"
)

//go:embed binaries/envoy/v1.33/x86_64/envoy
//go:embed binaries/envoy/v1.33/aarch64/envoy
var envoyBinaryFS embed.FS

// EnvoyVersion is the version of Envoy embedded in this build.
const EnvoyVersion = "v1.33"

// ExtractEnvoyBinary extracts the embedded Envoy binary to the data directory.
// Returns the path to the extracted binary.
func ExtractEnvoyBinary(p *paths.Paths) (string, error) {
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x86_64"
	} else if arch == "arm64" {
		arch = "aarch64"
	}

	embeddedPath := fmt.Sprintf("binaries/envoy/%s/%s/envoy", EnvoyVersion, arch)
	extractPath := p.EnvoyBinary(EnvoyVersion, arch)

	// Check if already extracted
	if _, err := os.Stat(extractPath); err == nil {
		return extractPath, nil
	}

	// Create directory
	if err := os.MkdirAll(filepath.Dir(extractPath), 0755); err != nil {
		return "", fmt.Errorf("create envoy binary dir: %w", err)
	}

	// Read embedded binary
	data, err := envoyBinaryFS.ReadFile(embeddedPath)
	if err != nil {
		return "", fmt.Errorf("read embedded envoy binary: %w", err)
	}

	// Write to filesystem
	if err := os.WriteFile(extractPath, data, 0755); err != nil {
		return "", fmt.Errorf("write envoy binary: %w", err)
	}

	return extractPath, nil
}

// GetEnvoyBinaryPath returns path to extracted binary, extracting if needed.
func GetEnvoyBinaryPath(p *paths.Paths) (string, error) {
	return ExtractEnvoyBinary(p)
}
