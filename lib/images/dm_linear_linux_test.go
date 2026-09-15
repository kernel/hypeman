//go:build linux

package images

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseDMLinearDependencies(t *testing.T) {
	deps := parseDMLinearDependencies("2 dependencies : (loop2) (loop1)", 2)
	require.Equal(t, []string{"/dev/loop2", "/dev/loop1"}, deps)
}

func TestIsDMDeviceMissing(t *testing.T) {
	require.True(t, isDMDeviceMissing(errors.New("Device does not exist.")))
	require.False(t, isDMDeviceMissing(errors.New("permission denied")))
}

func TestValidateDMName(t *testing.T) {
	for _, name := range []string{"hypeman-image-chain", "chain_123"} {
		require.NoError(t, validateDMName(name))
	}
	for _, name := range []string{"", ".", "..", "a/b", `a\\b`} {
		require.Error(t, validateDMName(name))
	}
}
