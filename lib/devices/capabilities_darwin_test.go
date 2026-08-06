//go:build darwin

package devices

import "testing"

func TestCapabilitiesDoesNotSupportVGPU(t *testing.T) {
	t.Parallel()

	if Capabilities().SupportsVGPU {
		t.Fatal("macOS should not report vGPU support")
	}
}
