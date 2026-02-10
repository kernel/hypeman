package system

import _ "embed"

// InitBinary contains the embedded init binary for guest VMs.
// This is built by the Makefile before the main binary is compiled.
// The init binary is a statically-linked Go program that runs as PID 1 in the guest VM.
// It matches the architecture of the host (VMs run on the same arch as the host).
//
//go:embed init/init
var InitBinary []byte
