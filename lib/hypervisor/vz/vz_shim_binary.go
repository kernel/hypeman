//go:build darwin

package vz

import _ "embed"

// vzShimBinary contains the embedded vz-shim binary.
// Built by the Makefile before the main binary is compiled.
//
//go:embed vz-shim/vz-shim
var vzShimBinary []byte
