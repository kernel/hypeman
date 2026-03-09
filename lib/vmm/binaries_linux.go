//go:build linux

package vmm

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/kernel/hypeman/lib/paths"
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

	extractDir := filepath.Dir(extractPath)
	if err := os.MkdirAll(extractDir, 0755); err != nil {
		return "", fmt.Errorf("create binaries dir: %w", err)
	}

	lockFile, err := os.OpenFile(extractPath+".lock", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return "", fmt.Errorf("open extraction lock: %w", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return "", fmt.Errorf("lock extraction: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	// Another process may have extracted it while we waited on the lock.
	if _, err := os.Stat(extractPath); err == nil {
		return extractPath, nil
	}

	// Read embedded binary
	data, err := binaryFS.ReadFile(embeddedPath)
	if err != nil {
		return "", fmt.Errorf("read embedded binary: %w", err)
	}

	tmpFile, err := os.CreateTemp(extractDir, "cloud-hypervisor-*")
	if err != nil {
		return "", fmt.Errorf("create temp binary: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return "", fmt.Errorf("write temp binary: %w", err)
	}
	if err := tmpFile.Chmod(0755); err != nil {
		_ = tmpFile.Close()
		return "", fmt.Errorf("chmod temp binary: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("close temp binary: %w", err)
	}
	if err := os.Rename(tmpPath, extractPath); err != nil {
		return "", fmt.Errorf("install binary: %w", err)
	}
	cleanupTmp = false

	return extractPath, nil
}

// GetBinaryPath returns path to extracted binary, extracting if needed
func GetBinaryPath(p *paths.Paths, version CHVersion) (string, error) {
	return ExtractBinary(p, version)
}
