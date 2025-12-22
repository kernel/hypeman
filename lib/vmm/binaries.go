package vmm

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/onkernel/hypeman/lib/paths"
)

//go:embed binaries/cloud-hypervisor/v48.0/x86_64/cloud-hypervisor
//go:embed binaries/cloud-hypervisor/v48.0/aarch64/cloud-hypervisor
//go:embed binaries/cloud-hypervisor/v49.0/x86_64/cloud-hypervisor
//go:embed binaries/cloud-hypervisor/v49.0/aarch64/cloud-hypervisor
var binaryFS embed.FS

type CHVersion string

const (
	V48_0 CHVersion = "v48.0"
	V49_0 CHVersion = "v49.0"
)

var SupportedVersions = []CHVersion{V48_0, V49_0}

// ExtractBinary extracts the embedded Cloud Hypervisor binary to the data directory
func ExtractBinary(p *paths.Paths, version CHVersion) (string, error) {
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x86_64"
	} else if arch == "arm64" {
		arch = "aarch64"
	}

	embeddedPath := fmt.Sprintf("binaries/cloud-hypervisor/%s/%s/cloud-hypervisor", version, arch)
	extractPath := p.SystemBinary(string(version), arch)

	// Check if already extracted
	if _, err := os.Stat(extractPath); err == nil {
		return extractPath, nil
	}

	// Create directory
	if err := os.MkdirAll(filepath.Dir(extractPath), 0755); err != nil {
		return "", fmt.Errorf("create binaries dir: %w", err)
	}

	// Read embedded binary
	data, err := binaryFS.ReadFile(embeddedPath)
	if err != nil {
		return "", fmt.Errorf("read embedded binary: %w", err)
	}

	// Write to filesystem
	if err := os.WriteFile(extractPath, data, 0755); err != nil {
		return "", fmt.Errorf("write binary: %w", err)
	}

	return extractPath, nil
}

// GetBinaryPath returns path to extracted binary, extracting if needed
func GetBinaryPath(p *paths.Paths, version CHVersion) (string, error) {
	return ExtractBinary(p, version)
}
