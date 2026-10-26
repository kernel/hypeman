package instances

import "fmt"

// GuestOperation names a host API operation whose availability depends on the
// guest OS and on the shared guest agent.
type GuestOperation string

const (
	GuestOpExec     GuestOperation = "exec"
	GuestOpFileCopy GuestOperation = "file copy"
	GuestOpStat     GuestOperation = "stat"
)

// GuestOperationUnsupportedReason returns an empty string when op is available for
// this instance, or the user-facing reason it is not. The host API uses it to decide
// which operations need the shared system agent. Readiness, probes and shutdown check
// GuestAgentEnabled directly, which is the same predicate.
func (s *StoredMetadata) GuestOperationUnsupportedReason(op GuestOperation) string {
	if s.MacOS != nil && !s.GuestAgentEnabled() {
		return fmt.Sprintf("%s requires the shared guest agent on macOS instances", op)
	}
	return ""
}
