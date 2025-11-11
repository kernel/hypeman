package vmm

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

var versionRegex = regexp.MustCompile(`v(\d+)\.(\d+)\.(\d+)`)

// ParseVersion extracts version from cloud-hypervisor --version output
func ParseVersion(binaryPath string) (CHVersion, error) {
	cmd := exec.Command(binaryPath, "--version")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("execute --version: %w", err)
	}

	versionStr := strings.TrimSpace(string(output))

	// Try to match known versions
	if strings.Contains(versionStr, "v48.0") {
		return V48_0, nil
	}
	if strings.Contains(versionStr, "v49.0") {
		return V49_0, nil
	}

	return "", fmt.Errorf("unsupported version: %s", versionStr)
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
