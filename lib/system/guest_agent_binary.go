package system

import _ "embed"

// GuestAgentBinary contains the embedded guest-agent binary
// This is built by the Makefile before the main binary is compiled
//go:embed guest_agent/guest-agent
var GuestAgentBinary []byte
