//go:build linux

package devices

import "testing"

func TestCapabilitiesSupportsVGPU(t *testing.T) {
	t.Parallel()

	if !Capabilities().SupportsVGPU {
		t.Fatal("Linux should report vGPU support")
	}
}
