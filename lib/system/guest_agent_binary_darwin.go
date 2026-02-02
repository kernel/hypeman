//go:build darwin

package system

import _ "embed"

// GuestAgentBinary contains the cross-compiled Linux guest agent for guest VMs.
// This is built by the Makefile with GOOS=linux before the main binary is compiled.
// The guest agent handles exec, file operations, and other guest-side functionality.
//
//go:embed guest_agent/guest-agent
var GuestAgentBinary []byte
