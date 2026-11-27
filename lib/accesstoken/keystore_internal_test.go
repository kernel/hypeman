package accesstoken

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A key set that could not be saved must not leave the older copy behind:
// that copy may list keys the authorization server has withdrawn, and a
// restart would trust them again.
func TestFailedSaveRemovesTheOlderCopy(t *testing.T) {
	store := NewFileKeyStore(t.TempDir(), slog.New(slog.DiscardHandler))
	id := strings.Repeat("a", 64)
	store.Save(id, "https://auth.example.test", []byte(`{"keys":["older"]}`))
	_, found := store.Load(id)
	require.True(t, found)

	replace := store.write
	store.write = func(string, []byte) error { return errors.New("no space left on device") }
	store.Save(id, "https://auth.example.test", []byte(`{"keys":[]}`))
	_, found = store.Load(id)
	require.False(t, found, "a restart has no copy and fetches the keys")
	// Nothing to remove is not a second failure.
	store.Save(id, "https://auth.example.test", []byte(`{"keys":[]}`))

	store.write = replace
	store.Save(id, "https://auth.example.test", []byte(`{"keys":["newer"]}`))
	keySet, found := store.Load(id)
	require.True(t, found)
	require.JSONEq(t, `{"keys":["newer"]}`, string(keySet))
}
