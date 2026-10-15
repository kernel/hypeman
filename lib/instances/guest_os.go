package instances

import "fmt"

// GuestOperation names a host API operation whose availability depends on the
// guest OS and on the shared guest agent.
type GuestOperation string

const (
	GuestOpExec     GuestOperation = "exec"
	GuestOpFileCopy GuestOperation = "file copy"
	GuestOpStat     GuestOperation = "stat"
	// GuestOpPortForward reaches a TCP port on a macOS guest's loopback interface.
	// Linux guests expose ports through ingress instead.
	GuestOpPortForward GuestOperation = "port forwarding"
)

// GuestOperationUnsupportedReason returns an empty string when op is available for
// this instance, or the user-facing reason it is not. It is the single rule for
// which operations need the shared system agent on each OS.
func (s *StoredMetadata) GuestOperationUnsupportedReason(op GuestOperation) string {
	if s.MacOS == nil {
		if op == GuestOpPortForward {
			return "port forwarding is only available for macOS guests; use ingress for Linux guests"
		}
		return ""
	}
	if !s.GuestAgentEnabled() {
		return fmt.Sprintf("%s requires the shared guest agent on macOS instances", op)
	}
	return ""
}
