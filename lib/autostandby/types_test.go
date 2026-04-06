package autostandby

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTCPStateConstantsMatchExpectedKernelValues(t *testing.T) {
	t.Parallel()

	assert.Equal(t, TCPState(10), TCPStateIgnore)
	assert.Equal(t, TCPState(11), TCPStateRetrans)
}

