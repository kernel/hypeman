package system

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseKernelVersion(t *testing.T) {
	version, ok := ParseKernelVersion(string(Kernel_202603301))
	assert.True(t, ok)
	assert.Equal(t, Kernel_202603301, version)

	_, ok = ParseKernelVersion("ch-6.12.8-kernel-9.9-20990101")
	assert.False(t, ok)
}
