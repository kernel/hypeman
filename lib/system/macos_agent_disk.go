package system

// MacOSAgentVolume is the volume name of the read-only guest agent disk attached
// to macOS guests. The launcher in a macOS image runs MacOSAgentBinary from it.
const (
	MacOSAgentVolume = "HypemanAgent"
	MacOSAgentBinary = "hypeman-guest-agent"
)
