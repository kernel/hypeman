package instances

import (
	"fmt"

	"github.com/kernel/hypeman/lib/guest"
)

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
// this instance, or the user-facing reason it is not. The host API uses it to decide
// which operations need the shared system agent. Readiness, probes and shutdown check
// GuestAgentEnabled directly, which is the same predicate.
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
	if op == GuestOpPortForward && !s.GuestAgent.HasCapability(guest.AgentCapability_AGENT_CAPABILITY_PORT_FORWARD) {
		// No agent info means readiness was recorded before agents reported it, by
		// an agent that predates forwarding. Restarting the guest records it.
		if s.GuestAgent == nil {
			return "the guest agent has not reported port forwarding support; restart the instance to record it"
		}
		return fmt.Sprintf("the guest agent (%q, protocol %d) does not offer port forwarding: its image predates it, or its forwarder could not start", s.GuestAgent.Version, s.GuestAgent.ProtocolVersion)
	}
	return ""
}
