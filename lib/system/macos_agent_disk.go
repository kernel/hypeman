package system

// MacOSAgentVolume is the volume name of the read-only guest agent disk attached
// to macOS guests. The launcher in a macOS image runs MacOSAgentBinary from it.
const (
	MacOSAgentVolume = "HypemanAgent"
	MacOSAgentBinary = "hypeman-guest-agent"
)

// A macOS guest created with env gets a second read-only disk, an ISO 9660 volume
// named MacOSConfigVolume holding MacOSConfigEnvFile, a JSON object of the env. On
// every boot the guest agent copies it to MacOSGuestEnvFile (root only) and unmounts
// the volume. The guest agent keeps its own copy of these names.
const (
	MacOSConfigVolume  = "HypemanConfig"
	MacOSConfigEnvFile = "env.json"
	MacOSGuestEnvFile  = "/var/run/hypeman/env.json"
)
