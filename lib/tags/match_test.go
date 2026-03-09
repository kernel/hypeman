package tags

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMatches(t *testing.T) {
	t.Parallel()

	resource := map[string]string{"team": "backend", "env": "prod"}

	require.True(t, Matches(resource, nil))
	require.True(t, Matches(resource, map[string]string{}))
	require.True(t, Matches(resource, map[string]string{"team": "backend"}))
	require.True(t, Matches(resource, map[string]string{"team": "backend", "env": "prod"}))
	require.False(t, Matches(resource, map[string]string{"team": "frontend"}))
	require.False(t, Matches(nil, map[string]string{"team": "backend"}))
	require.False(t, Matches(map[string]string{}, map[string]string{"team": "backend"}))
}
