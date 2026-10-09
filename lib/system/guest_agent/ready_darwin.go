//go:build darwin

package main

// defaultReadyFilePath is where the guest agent records readiness when
// HYPEMAN_AGENT_READY_FILE is unset.
const defaultReadyFilePath = "/var/run/hypeman/guest-agent-ready"
