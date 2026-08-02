package volumes

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReservedVolumeIDPrefix(t *testing.T) {
	assert.Equal(t, "build-cache-", ReservedVolumeIDPrefix("build-cache-abc"))
	assert.Equal(t, "build-disk-", ReservedVolumeIDPrefix("build-disk-123"))
	assert.Equal(t, "build-source-", ReservedVolumeIDPrefix("build-source-123"))
	assert.Equal(t, "build-config-", ReservedVolumeIDPrefix("build-config-123"))
	assert.Empty(t, ReservedVolumeIDPrefix("my-data"))
	assert.Empty(t, ReservedVolumeIDPrefix("build-caches"))
}

func TestReservedTagNamespace(t *testing.T) {
	assert.Equal(t, SystemTagNamespace, ReservedTagNamespace("hypeman.system/managed-by"))
	assert.Equal(t, SystemTagNamespace, ReservedTagNamespace("hypeman.system/anything"))
	assert.Empty(t, ReservedTagNamespace("managed-by"))
	assert.Empty(t, ReservedTagNamespace("hypeman.system"))
	assert.Empty(t, ReservedTagNamespace("team"))
}
