package instances

import (
	"testing"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/images"
	"github.com/stretchr/testify/require"
)

func TestGuestOperationCapabilityTable(t *testing.T) {
	linux := &StoredMetadata{}
	for _, op := range []GuestOperation{GuestOpExec, GuestOpFileCopy, GuestOpStat} {
		require.Empty(t, linux.GuestOperationUnsupportedReason(op), "Linux guests keep their historical behavior")
	}

	withoutAgent := &StoredMetadata{MacOS: &images.MacOSImage{GuestAgent: false}, SkipGuestAgent: true}
	require.Contains(t, withoutAgent.GuestOperationUnsupportedReason(GuestOpExec), "requires the shared guest agent")

	withAgent := &StoredMetadata{MacOS: &images.MacOSImage{GuestAgent: true}}
	for _, op := range []GuestOperation{GuestOpExec, GuestOpFileCopy, GuestOpStat} {
		require.Empty(t, withAgent.GuestOperationUnsupportedReason(op))
	}
}

func TestPortForwardRequiresAgentCapability(t *testing.T) {
	agent := func(info *GuestAgentInfo) *StoredMetadata {
		return &StoredMetadata{MacOS: &images.MacOSImage{GuestAgent: true}, GuestAgent: info}
	}
	require.Empty(t, agent(&GuestAgentInfo{ProtocolVersion: 1, Capabilities: []string{guest.AgentCapability_AGENT_CAPABILITY_PORT_FORWARD.String()}}).GuestOperationUnsupportedReason(GuestOpPortForward))
	require.Contains(t, agent(&GuestAgentInfo{}).GuestOperationUnsupportedReason(GuestOpPortForward), "does not support port forwarding", "an agent that predates the capability is refused")
	require.Empty(t, agent(&GuestAgentInfo{}).GuestOperationUnsupportedReason(GuestOpExec), "other operations do not need it")
	require.Empty(t, agent(nil).GuestOperationUnsupportedReason(GuestOpPortForward), "an agent recorded before GetAgentInfo is not checked")
	require.Contains(t, (&StoredMetadata{}).GuestOperationUnsupportedReason(GuestOpPortForward), "use ingress")
}
