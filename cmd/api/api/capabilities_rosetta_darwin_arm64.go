//go:build darwin && arm64

package api

import "github.com/Code-Hex/vz/v3"

// rosettaInstalled reports whether Rosetta translation for Linux guests is
// installed and usable right now, using the same Virtualization.framework
// probe the vz-shim enforces when a launch requests Rosetta
// (cmd/vz-shim/rosetta_arm64.go): NotInstalled (Rosetta missing) and
// NotSupported (macOS < 13) both fail launches, so capability reporting must
// not advertise emulation in either state. Evaluated per request, so
// installing Rosetta (softwareupdate --install-rosetta) is reflected without
// a restart. cmd/api already links Virtualization.framework on macOS
// (checkHypervisorAccess), so this adds no build or runtime requirement.
var rosettaInstalled = func() bool {
	return vz.LinuxRosettaDirectoryShareAvailability() == vz.LinuxRosettaAvailabilityInstalled
}
