package vmm

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// versionRegex extracts a vN.N or vN.N.N version tag from --version output.
var versionRegex = regexp.MustCompile(`v\d+\.\d+(?:\.\d+)?`)

// ParseVersion extracts version from cloud-hypervisor --version output
// and matches it against SupportedVersions.
func ParseVersion(binaryPath string) (CHVersion, error) {
	cmd := exec.Command(binaryPath, "--version")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("execute --version: %w", err)
	}

	versionStr := strings.TrimSpace(string(output))

	match := versionRegex.FindString(versionStr)
	if match != "" {
		for _, v := range SupportedVersions {
			if match == string(v) {
				return v, nil
			}
		}
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
