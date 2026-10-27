package main

import (
	"context"

	pb "github.com/kernel/hypeman/lib/guest"
)

// agentProtocolVersion is reported to the host. Bump it only for a change an older
// host cannot handle; report additive features as capabilities instead.
const agentProtocolVersion = 1

// agentVersion identifies the build in diagnostics. Builds set it with
// -ldflags "-X main.agentVersion=...".
var agentVersion = "dev"

// agentCapabilities lists optional features this agent supports.
func agentCapabilities() []pb.AgentCapability {
	if portForwarding.Load() {
		return []pb.AgentCapability{pb.AgentCapability_AGENT_CAPABILITY_PORT_FORWARD}
	}
	return nil
}

func (s *guestServer) GetAgentInfo(context.Context, *pb.GetAgentInfoRequest) (*pb.GetAgentInfoResponse, error) {
	return &pb.GetAgentInfoResponse{
		ProtocolVersion: agentProtocolVersion,
		Version:         agentVersion,
		Capabilities:    agentCapabilities(),
	}, nil
}
