package paths

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidatePathComponent(t *testing.T) {
	for _, value := range []string{"snapshot-1", "team_cache"} {
		require.NoError(t, ValidatePathComponent(value))
	}
	for _, value := range []string{"", ".", "..", "../outside", "nested/resource", "/absolute"} {
		require.ErrorIs(t, ValidatePathComponent(value), ErrInvalidPathComponent)
	}
}
