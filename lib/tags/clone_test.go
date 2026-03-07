package tags

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClone(t *testing.T) {
	t.Parallel()

	require.Nil(t, Clone(nil))
	require.Nil(t, Clone(map[string]string{}))

	in := map[string]string{"team": "backend"}
	out := Clone(in)
	require.Equal(t, in, out)

	out["team"] = "frontend"
	require.Equal(t, "backend", in["team"])
}
