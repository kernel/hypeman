package guestmemory

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMergeKernelArgs(t *testing.T) {
	merged := MergeKernelArgs("console=ttyS0 foo=1", "foo=2", "init_on_alloc=0 init_on_free=0")
	assert.Equal(t, "console=ttyS0 foo=2 init_on_alloc=0 init_on_free=0", merged)
}
