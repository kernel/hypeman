package tags

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	require.NoError(t, Validate(nil))
	require.NoError(t, Validate(map[string]string{}))
	require.NoError(t, Validate(map[string]string{"team": "backend", "desc": ""}))
	require.NoError(t, Validate(map[string]string{"a+b": "x/y:z@w="}))

	err := Validate(map[string]string{"": "x"})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMetadata)

	err = Validate(map[string]string{"tēam": "backend"})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMetadata)

	err = Validate(map[string]string{"team": "支付"})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMetadata)

	tooMany := make(map[string]string, MaxEntries+1)
	for i := 0; i < MaxEntries+1; i++ {
		tooMany[fmt.Sprintf("k%d", i)] = "v"
	}
	err = Validate(tooMany)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMetadata)

	longKey := map[string]string{strings.Repeat("a", MaxKeyLength+1): "v"}
	err = Validate(longKey)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMetadata)

	longValue := map[string]string{"key": strings.Repeat("a", MaxValueLength+1)}
	err = Validate(longValue)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMetadata)
}
