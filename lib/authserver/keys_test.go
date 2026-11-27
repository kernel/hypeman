package authserver

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

var discardLog = slog.New(slog.DiscardHandler)

func encodePEM(t *testing.T, kind string, der []byte, err error) []byte {
	t.Helper()
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
}

func newSigningKey(t *testing.T) *SigningKey {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(private)
	key, err := ParseSigningKey(encodePEM(t, "PRIVATE KEY", der, err))
	require.NoError(t, err)
	return key
}

func TestSigningKeyFormsAndItsID(t *testing.T) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(private)
	first, err := ParseSigningKey(encodePEM(t, "PRIVATE KEY", pkcs8, err))
	require.NoError(t, err)
	sec1, err := x509.MarshalECPrivateKey(private)
	second, err := ParseSigningKey(encodePEM(t, "EC PRIVATE KEY", sec1, err))
	require.NoError(t, err)
	require.Len(t, first.ID, 43, "an RFC 7638 thumbprint")
	require.Equal(t, first.ID, second.ID, "the ID follows the key, not its encoding")
	require.NotEqual(t, first.ID, newSigningKey(t).ID)

	pkix, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	publicPEM := encodePEM(t, "PUBLIC KEY", pkix, err)
	public, err := ParsePublicKey(publicPEM)
	require.NoError(t, err)
	require.True(t, public.Equal(&private.PublicKey))

	larger, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	largerDER, err := x509.MarshalPKCS8PrivateKey(larger)
	largerPEM := encodePEM(t, "PRIVATE KEY", largerDER, err)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	rsaDER, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	rsaPEM := encodePEM(t, "PRIVATE KEY", rsaDER, err)
	for name, encoded := range map[string][]byte{"another curve": largerPEM, "another algorithm": rsaPEM, "a public key": publicPEM, "not PEM": []byte("secret"), "not a key": encodePEM(t, "PRIVATE KEY", []byte("x"), nil)} {
		_, err := ParseSigningKey(encoded)
		require.ErrorIs(t, err, ErrKey, name)
	}
	largerPKIX, err := x509.MarshalPKIXPublicKey(&larger.PublicKey)
	for name, encoded := range map[string][]byte{"another curve": encodePEM(t, "PUBLIC KEY", largerPKIX, err), "a private key": encodePEM(t, "PRIVATE KEY", pkcs8, nil), "not PEM": []byte("key")} {
		_, err := ParsePublicKey(encoded)
		require.ErrorIs(t, err, ErrKey, name)
	}
}

// The key set lists the signing key first and each published key once, and
// never anything private.
func TestKeySetPublishesOnlyPublicKeys(t *testing.T) {
	signing, previous := newSigningKey(t), newSigningKey(t)
	raw, err := keySet(signing, []*ecdsa.PublicKey{&previous.private.PublicKey, &signing.private.PublicKey, &previous.private.PublicKey})
	require.NoError(t, err)
	var set struct {
		Keys []map[string]string `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(raw, &set))
	require.Len(t, set.Keys, 2, "each key is listed once, however often it is configured")
	require.Equal(t, signing.ID, set.Keys[0]["kid"])
	require.Equal(t, previous.ID, set.Keys[1]["kid"])
	for _, key := range set.Keys {
		require.Equal(t, "EC", key["kty"])
		require.Equal(t, "P-256", key["crv"])
		require.Equal(t, "ES256", key["alg"])
		require.Equal(t, "sig", key["use"])
		require.NotContains(t, key, "d")
	}

	var many []*ecdsa.PublicKey
	for range maxKeys {
		many = append(many, &newSigningKey(t).private.PublicKey)
	}
	_, err = keySet(signing, many)
	require.ErrorIs(t, err, ErrKey, "more keys than a Hypeman server accepts")
}
