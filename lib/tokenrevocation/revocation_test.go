package tokenrevocation

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestList(t *testing.T) {
	l, err := New([]string{strings.ToUpper(Hash("revoked-token")) + " "})
	require.NoError(t, err)

	assert.True(t, l.IsRevoked("revoked-token"))
	assert.False(t, l.IsRevoked("other-token"))
	assert.Equal(t, 1, l.Len())
}

func TestNilListRevokesNothing(t *testing.T) {
	var l *List
	assert.False(t, l.IsRevoked("any-token"))
	assert.Equal(t, 0, l.Len())
}

func TestNewRejectsInvalidHashes(t *testing.T) {
	for _, h := range []string{"", "not-hex", Hash("x")[:62], "revoked-token"} {
		_, err := New([]string{h})
		assert.Error(t, err, "hash %q", h)
	}

	_, err := New([]string{"raw-token-pasted-by-mistake"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "raw-token-pasted-by-mistake")
}
