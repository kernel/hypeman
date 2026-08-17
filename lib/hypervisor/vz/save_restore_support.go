package vz

import (
	"strconv"
	"strings"
)

// saveRestoreMinMacOSMajor is the minimum macOS major version whose
// Virtualization.framework exposes VM save/restore (VZVirtualMachine
// saveMachineStateToURL / restoreMachineStateFromURL).
const saveRestoreMinMacOSMajor = 14

// SaveRestoreSupported reports whether Virtualization.framework VM
// save/restore (snapshots, and therefore standby) is available on a host with
// the given GOOS, GOARCH, and macOS product version (e.g. "14.5.1"). The
// product version is only consulted on darwin/arm64; an empty or unparsable
// version is treated as unsupported so a failed probe never overstates
// support.
func SaveRestoreSupported(goos, goarch, productVersion string) bool {
	if goos != "darwin" || goarch != "arm64" {
		return false
	}
	major, ok := parseMacOSMajorVersion(productVersion)
	return ok && major >= saveRestoreMinMacOSMajor
}

// parseMacOSMajorVersion extracts the major component from a macOS product
// version string like "14", "14.5", or "14.5.1".
func parseMacOSMajorVersion(productVersion string) (int, bool) {
	productVersion = strings.TrimSpace(productVersion)
	if productVersion == "" {
		return 0, false
	}
	majorStr, _, _ := strings.Cut(productVersion, ".")
	major, err := strconv.Atoi(majorStr)
	if err != nil || major < 0 {
		return 0, false
	}
	return major, true
}
