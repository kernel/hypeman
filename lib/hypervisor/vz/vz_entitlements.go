//go:build darwin

package vz

import _ "embed"

// vzEntitlements contains the embedded vz.entitlements plist file.
// This is used at runtime to codesign the extracted vz-shim binary.
//
//go:embed vz.entitlements
var vzEntitlements []byte
