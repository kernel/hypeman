package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kernel/hypeman/lib/builds"
	"github.com/stretchr/testify/require"
)

func TestMachineRecipeHashInBuildResponse(t *testing.T) {
	for _, hash := range []string{"", strings.Repeat("a", 64)} {
		response := buildToOAPI(&builds.Build{Provenance: &builds.BuildProvenance{RecipeHash: hash}})
		require.NotNil(t, response.Provenance)
		data, err := json.Marshal(response)
		require.NoError(t, err)
		if hash == "" {
			require.Nil(t, response.Provenance.RecipeHash)
			require.NotContains(t, string(data), "recipe_hash")
		} else {
			require.Equal(t, hash, *response.Provenance.RecipeHash)
			require.Contains(t, string(data), `"recipe_hash":"`+hash+`"`)
		}
	}
}
