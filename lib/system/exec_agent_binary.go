package system

import _ "embed"

// ExecAgentBinary contains the embedded exec-agent binary
// This is built by the Makefile before the main binary is compiled
//go:embed exec_agent/exec-agent
var ExecAgentBinary []byte

