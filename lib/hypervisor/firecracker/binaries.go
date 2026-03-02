package firecracker

import (
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/kernel/hypeman/lib/paths"
)

type Version string

const (
	V1_14_2 Version = "v1.14.2"
)

const defaultVersion = V1_14_2

var supportedVersions = []Version{
	V1_14_2,
}

//go:embed binaries
var binaryFS embed.FS

var (
	customBinaryPathMu sync.RWMutex
	customBinaryPath   string
)

var versionRegex = regexp.MustCompile(`v?\d+\.\d+\.\d+`)

// SetCustomBinaryPath configures a runtime override for the firecracker binary.
// When set, this path always takes precedence over embedded binaries.
func SetCustomBinaryPath(path string) {
	customBinaryPathMu.Lock()
	defer customBinaryPathMu.Unlock()
	customBinaryPath = strings.TrimSpace(path)
}

func getCustomBinaryPath() string {
	customBinaryPathMu.RLock()
	defer customBinaryPathMu.RUnlock()
	return customBinaryPath
}

func resolveBinaryPath(p *paths.Paths, version string) (string, error) {
	if path := getCustomBinaryPath(); path != "" {
		if err := validateExecutable(path); err != nil {
			return "", fmt.Errorf("invalid firecracker custom binary path %q: %w", path, err)
		}
		return path, nil
	}

	if p == nil {
		return "", fmt.Errorf("paths are required when using embedded firecracker binaries")
	}

	return extractBinary(p, parseVersion(version))
}

func parseVersion(version string) Version {
	if version == "" {
		return defaultVersion
	}
	for _, supported := range supportedVersions {
		if version == string(supported) {
			return supported
		}
	}
	return defaultVersion
}

func extractBinary(p *paths.Paths, version Version) (string, error) {
	arch, err := normalizeArch()
	if err != nil {
		return "", err
	}

	embeddedPath := filepath.ToSlash(filepath.Join("binaries", "firecracker", string(version), arch, "firecracker"))
	extractPath := p.FirecrackerBinary(string(version), arch)

	if err := validateExecutable(extractPath); err == nil {
		return extractPath, nil
	}

	data, err := binaryFS.ReadFile(embeddedPath)
	if err != nil {
		return "", fmt.Errorf("embedded firecracker binary not found at %s (run `make download-firecracker-binaries` or set hypervisor.firecracker_binary_path): %w", embeddedPath, err)
	}

	if err := os.MkdirAll(filepath.Dir(extractPath), 0755); err != nil {
		return "", fmt.Errorf("create firecracker binary directory: %w", err)
	}
	if err := os.WriteFile(extractPath, data, 0755); err != nil {
		return "", fmt.Errorf("write firecracker binary: %w", err)
	}

	return extractPath, nil
}

func detectVersion(binaryPath string) (string, error) {
	cmd := exec.Command(binaryPath, "--version")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("run firecracker --version: %w", err)
	}

	match := versionRegex.FindString(string(out))
	if match == "" {
		return "", fmt.Errorf("could not parse firecracker version from output: %s", strings.TrimSpace(string(out)))
	}
	if !strings.HasPrefix(match, "v") {
		match = "v" + match
	}
	return match, nil
}

func normalizeArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64", nil
	case "arm64":
		return "aarch64", nil
	default:
		return "", fmt.Errorf("unsupported architecture: %s", runtime.GOARCH)
	}
}

func validateExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("path is a directory")
	}
	if info.Mode()&0111 == 0 {
		return fmt.Errorf("file is not executable")
	}
	return nil
}
