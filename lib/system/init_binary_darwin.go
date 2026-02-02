//go:build darwin

package system

import _ "embed"

// InitBinary contains the cross-compiled Linux init binary for guest VMs.
// This is built by the Makefile with GOOS=linux before the main binary is compiled.
// The init binary is a statically-linked Go program that runs as PID 1 in the guest VM.
//
//go:embed init/init
var InitBinary []byte
