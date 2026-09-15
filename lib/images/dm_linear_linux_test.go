//go:build linux

package images

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateDMName(t *testing.T) {
	for _, name := range []string{"hypeman-image-chain", "chain_123"} {
		require.NoError(t, validateDMName(name))
	}
	for _, name := range []string{"", ".", "..", "a/b", `a\\b`} {
		require.Error(t, validateDMName(name))
	}
}
