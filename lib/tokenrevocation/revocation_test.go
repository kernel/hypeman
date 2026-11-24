package tokenrevocation

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"math/big"
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

// An ECDSA signature (r, s) verifies as (r, n-s) too, so every token signed
// that way has a second spelling its holder can compute. Revoking either
// revokes both.
func TestBothTokensOfAnECDSASignatureAreRevoked(t *testing.T) {
	for _, tc := range []struct {
		method *jwt.SigningMethodECDSA
		curve  elliptic.Curve
	}{
		{jwt.SigningMethodES256, elliptic.P256()},
		{jwt.SigningMethodES384, elliptic.P384()},
		{jwt.SigningMethodES512, elliptic.P521()},
	} {
		t.Run(tc.method.Alg(), func(t *testing.T) {
			key, err := ecdsa.GenerateKey(tc.curve, rand.Reader)
			require.NoError(t, err)
			token, err := jwt.NewWithClaims(tc.method, jwt.MapClaims{"sub": "user"}).SignedString(key)
			require.NoError(t, err)

			// Build the other token here, apart from the code under test.
			i := strings.LastIndexByte(token, '.')
			signature, err := base64.RawURLEncoding.DecodeString(token[i+1:])
			require.NoError(t, err)
			half := len(signature) / 2
			s := new(big.Int).SetBytes(signature[half:])
			new(big.Int).Sub(tc.curve.Params().N, s).FillBytes(signature[half:])
			twin := token[:i+1] + base64.RawURLEncoding.EncodeToString(signature)
			require.NotEqual(t, token, twin)
			for _, spelling := range []string{token, twin} {
				_, err = jwt.Parse(spelling, func(*jwt.Token) (interface{}, error) { return &key.PublicKey, nil })
				require.NoError(t, err, "both must verify, otherwise the test proves nothing")
			}
			require.NotEqual(t, Hash(token), Hash(twin))

			for _, listed := range []string{token, twin} {
				l, err := New([]string{Hash(listed)})
				require.NoError(t, err)
				assert.True(t, l.IsRevoked(token))
				assert.True(t, l.IsRevoked(twin))
				assert.True(t, l.IsRevoked(twin[:len(twin)-5]+"\n"+twin[len(twin)-5:]), "another spelling of the other token")
			}
			other, err := jwt.NewWithClaims(tc.method, jwt.MapClaims{"sub": "other"}).SignedString(key)
			require.NoError(t, err)
			l, err := New([]string{Hash(token)})
			require.NoError(t, err)
			assert.False(t, l.IsRevoked(other))
		})
	}
}

// The algorithm is the header member named exactly "alg", the one
// verification reads. Another capitalization beside it changes nothing.
func TestTheSecondFormFollowsTheExactAlgorithmMember(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	encode := base64.RawURLEncoding.EncodeToString
	input := encode([]byte(`{"alg":"ES256","typ":"JWT","ALG":"HS256","Alg":"none"}`)) + "." + encode([]byte(`{"sub":"user"}`))
	signature, err := jwt.SigningMethodES256.Sign(input, key)
	require.NoError(t, err)
	token := input + "." + encode(signature)
	_, err = jwt.Parse(token, func(*jwt.Token) (interface{}, error) { return &key.PublicKey, nil }, jwt.WithValidMethods([]string{"ES256"}))
	require.NoError(t, err, "it must verify, otherwise the test proves nothing")

	twin, ok := ecdsaTwin(token)
	require.True(t, ok)
	_, err = jwt.Parse(twin, func(*jwt.Token) (interface{}, error) { return &key.PublicKey, nil }, jwt.WithValidMethods([]string{"ES256"}))
	require.NoError(t, err)
	l, err := New([]string{Hash(token)})
	require.NoError(t, err)
	assert.True(t, l.IsRevoked(twin))
}

// Only an ECDSA signature has a second form. Other tokens are looked up by
// their own hash alone, whatever their header claims.
func TestOtherTokensHaveNoSecondForm(t *testing.T) {
	_, ok := ecdsaTwin(signedToken(t))
	assert.False(t, ok)
	for _, token := range []string{"", "revoked-token", "a.b.c", "e30.e30.", "eyJhbGciOiJFUzI1NiJ9", "eyJhbGciOiJFUzI1NiJ9.e30.AAAA"} {
		_, ok := ecdsaTwin(token)
		assert.False(t, ok, token)
	}
}
