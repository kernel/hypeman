//go:build darwin

package vz

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClientCapabilities(t *testing.T) {
	// Create a mock client (without actual socket connection)
	// We can't create a real client without a running shim
	c := &Client{
		socketPath: "/nonexistent/socket",
		httpClient: nil, // Will fail if actually used
	}

	caps := c.Capabilities()

	// Verify expected capabilities
	assert.False(t, caps.SupportsSnapshot, "Snapshot not supported: Virtualization.framework limitation for Linux guests")
	assert.True(t, caps.SupportsPause, "vz supports pause")
	assert.True(t, caps.SupportsVsock, "vz supports vsock")
	assert.False(t, caps.SupportsHotplugMemory, "vz does not support memory hotplug")
	assert.False(t, caps.SupportsGPUPassthrough, "vz does not support GPU passthrough")
	assert.False(t, caps.SupportsDiskIOLimit, "vz does not support disk I/O limits")
}

func TestVzMetadataStructure(t *testing.T) {
	// Test that vzMetadata can be unmarshaled from stored instance metadata
	metadataJSON := `{
		"Image": "alpine:3.20",
		"Vcpus": 2,
		"Size": 1073741824,
		"KernelVersion": "ch-6.12.8-kernel-1.3-202601152",
		"MAC": "02:00:00:34:49:ae"
	}`

	var metadata vzMetadata
	err := json.Unmarshal([]byte(metadataJSON), &metadata)
	assert.NoError(t, err)
	assert.Equal(t, "alpine:3.20", metadata.Image)
	assert.Equal(t, 2, metadata.VCPUs)
	assert.Equal(t, int64(1073741824), metadata.Size)
	assert.Equal(t, "ch-6.12.8-kernel-1.3-202601152", metadata.KernelVersion)
	assert.Equal(t, "02:00:00:34:49:ae", metadata.MAC)
}
