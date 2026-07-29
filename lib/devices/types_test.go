package devices

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVirtualFunctionAllocationCompatibility(t *testing.T) {
	t.Parallel()

	legacy := VirtualFunction{HasMdev: true}
	assert.True(t, legacy.IsAllocated())

	vf := VirtualFunction{Allocated: true, HasMdev: true}
	data, err := json.Marshal(vf)
	require.NoError(t, err)
	assert.JSONEq(t, `{"pci_address":"","parent_gpu":"","allocated":true,"has_mdev":true}`, string(data))
}
