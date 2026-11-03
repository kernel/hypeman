package tokenrevocation

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
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

func TestHashOfIssuedTokenMatchesRawSHA256(t *testing.T) {
	token := signedToken(t)
	sum := sha256.Sum256([]byte(token))
	assert.Equal(t, hex.EncodeToString(sum[:]), Hash(token))
}

// Equivalent signature spellings verify with the JWT parser and must stay revoked.
func TestEquivalentSignatureEncodingsStayRevoked(t *testing.T) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	token := signedToken(t)
	l, err := New([]string{Hash(token)})
	require.NoError(t, err)

	last := strings.IndexByte(alphabet, token[len(token)-1])
	variants := map[string]string{
		"unused trailing bits set": token[:len(token)-1] + string(alphabet[last^1]),
		"embedded newline":         token[:len(token)-5] + "\n" + token[len(token)-5:],
		"embedded carriage return": token[:len(token)-5] + "\r" + token[len(token)-5:],
	}
	for name, variant := range variants {
		t.Run(name, func(t *testing.T) {
			_, err := jwt.Parse(variant, func(*jwt.Token) (interface{}, error) { return []byte(testSecret), nil })
			require.NoError(t, err, "variant should verify, otherwise the test proves nothing")
			assert.True(t, l.IsRevoked(variant))
		})
	}
}

const testSecret = "test-secret"

func signedToken(t *testing.T) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "user"}).SignedString([]byte(testSecret))
	require.NoError(t, err)
	return token
}
