//go:build linux

package main

// defaultReadyFilePath is where the guest agent records readiness when
// HYPEMAN_AGENT_READY_FILE is unset.
const defaultReadyFilePath = "/run/hypeman/guest-agent-ready"
