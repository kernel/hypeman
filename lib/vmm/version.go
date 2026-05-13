package vmm

import (
	"fmt"
	"os/exec"
	"strings"
)

// ParseVersion extracts version from cloud-hypervisor --version output
// and matches it against SupportedVersions. Checks longer version strings
// first so "v49.0.1" doesn't falsely match "v49.0".
func ParseVersion(binaryPath string) (CHVersion, error) {
	cmd := exec.Command(binaryPath, "--version")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("execute --version: %w", err)
	}

	versionStr := strings.TrimSpace(string(output))

	// Match against supported versions, longest first to avoid
	// "v49.0" matching inside "v49.0.1".
	for _, v := range sortedByLengthDesc(SupportedVersions) {
		if strings.Contains(versionStr, string(v)) {
			return v, nil
		}
	}

	return "", fmt.Errorf("unsupported version: %s", versionStr)
}

func sortedByLengthDesc(versions []CHVersion) []CHVersion {
	sorted := make([]CHVersion, len(versions))
	copy(sorted, versions)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if len(sorted[j]) > len(sorted[i]) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	return sorted
}

// IsVersionSupported checks if a version is supported by this library
func IsVersionSupported(version CHVersion) bool {
	for _, v := range SupportedVersions {
		if v == version {
			return true
		}
	}
	return false
}
