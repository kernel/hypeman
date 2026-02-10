//go:build darwin

package vz

import _ "embed"

//go:embed vz.entitlements
var vzEntitlements []byte
