//go:build !(darwin && arm64)

package api

// rosettaInstalled is never true off Apple Silicon macOS: Rosetta emulation
// exists only under vz. Kept as a var with the same shape as the
// darwin/arm64 probe so the handler code is identical on every platform.
var rosettaInstalled = func() bool { return false }
