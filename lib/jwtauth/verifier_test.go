package jwtauth

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestVerifierRotation(t *testing.T) {
	for _, phase := range []struct {
		name     string
		primary  string
		fallback []string
		accepted []string
	}{
		{"before", "old", nil, []string{"old"}},
		{"overlap", "old", []string{"new", "another"}, []string{"old", "new", "another"}},
		{"switched", "new", []string{"old"}, []string{"old", "new"}},
		{"retired", "new", nil, []string{"new"}},
	} {
		t.Run(phase.name, func(t *testing.T) {
			verifier, err := NewVerifier(phase.primary, phase.fallback, noop.NewMeterProvider().Meter("test"))
			require.NoError(t, err)
			for _, key := range []string{"old", "new", "another", "unknown", ""} {
				for _, method := range []*jwt.SigningMethodHMAC{jwt.SigningMethodHS256, jwt.SigningMethodHS384, jwt.SigningMethodHS512} {
					token, err := jwt.NewWithClaims(method, jwt.MapClaims{
						"sub": "user", "exp": time.Now().Add(time.Hour).Unix(),
					}).SignedString([]byte(key))
					require.NoError(t, err)
					claims := jwt.MapClaims{}
					_, err = verifier.ParseWithClaims(context.Background(), token, claims)
					if slices.Contains(phase.accepted, key) {
						require.NoError(t, err)
						assert.Equal(t, "user", claims["sub"])
					} else {
						assert.ErrorIs(t, err, jwt.ErrTokenSignatureInvalid)
					}
				}
			}
		})
	}
}

func TestVerifierRejectsInvalidTokens(t *testing.T) {
	verifier, err := NewVerifier("new", []string{"old"}, noop.NewMeterProvider().Meter("test"))
	require.NoError(t, err)
	for _, key := range []string{"old", "new"} {
		for _, tc := range []struct {
			name   string
			claims jwt.MapClaims
			err    error
		}{
			{"expired", jwt.MapClaims{"exp": time.Now().Add(-time.Hour).Unix()}, jwt.ErrTokenExpired},
			{"future", jwt.MapClaims{"nbf": time.Now().Add(time.Hour).Unix()}, jwt.ErrTokenNotValidYet},
			{"invalid expiry", jwt.MapClaims{"exp": "tomorrow"}, jwt.ErrTokenInvalidClaims},
		} {
			t.Run(key+"/"+tc.name, func(t *testing.T) {
				token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, tc.claims).SignedString([]byte(key))
				require.NoError(t, err)
				_, err = verifier.ParseWithClaims(context.Background(), token, jwt.MapClaims{})
				assert.ErrorIs(t, err, tc.err)
			})
		}
	}
	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"sub": "user"}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)
	for _, token := range []string{unsigned, "not.a.token", ""} {
		_, err := verifier.ParseWithClaims(context.Background(), token, jwt.MapClaims{})
		assert.Error(t, err)
	}
}

func TestVerifierRejectsEmptySecrets(t *testing.T) {
	for _, tc := range []struct {
		primary  string
		fallback []string
	}{
		{"", nil},
		{"", []string{"old"}},
		{"new", []string{"old", ""}},
	} {
		_, err := NewVerifier(tc.primary, tc.fallback, noop.NewMeterProvider().Meter("test"))
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "old")
	}
}

func TestVerifierMetricsConcurrent(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	verifier, err := NewVerifier("new", []string{"old"}, provider.Meter("test"))
	require.NoError(t, err)

	var wg sync.WaitGroup
	for _, key := range []string{"new", "old", "unknown"} {
		for _, ttl := range []time.Duration{time.Hour, -time.Hour} {
			token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
				"exp": time.Now().Add(ttl).Unix(),
			}).SignedString([]byte(key))
			require.NoError(t, err)
			for range 10 {
				wg.Go(func() {
					_, _ = verifier.ParseWithClaims(context.Background(), token, jwt.MapClaims{})
				})
			}
		}
	}
	wg.Wait()
	var metrics metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &metrics))
	require.Len(t, metrics.ScopeMetrics, 1)
	require.Len(t, metrics.ScopeMetrics[0].Metrics, 1)
	m := metrics.ScopeMetrics[0].Metrics[0]
	assert.Equal(t, "hypeman_jwt_validations_total", m.Name)
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, sum.DataPoints, 2)
	counts := map[string]int64{}
	for _, point := range sum.DataPoints {
		require.Equal(t, 1, point.Attributes.Len())
		role, ok := point.Attributes.Value("key_role")
		require.True(t, ok)
		counts[role.AsString()] = point.Value
	}
	assert.Equal(t, map[string]int64{"primary": 10, "fallback": 10}, counts)
}

type federationRules map[string]string

func (f federationRules) Current(issuer, rule, revision string) bool {
	return f[rule] == issuer+"@"+revision
}

// A federated token is bound to its rule's current configuration and never
// reads as a legacy full-access token.
func TestFederatedTokensFollowTheirRule(t *testing.T) {
	const secret = "primary"
	base, err := NewVerifier(secret, nil, noop.NewMeterProvider().Meter("test"))
	require.NoError(t, err)
	verifier := base.WithFederation(federationRules{"ci": "github@rev1"})
	sign := func(claims jwt.MapClaims) string {
		claims["exp"] = time.Now().Add(time.Hour).Unix()
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
		require.NoError(t, err)
		return token
	}
	federated := func(alter func(jwt.MapClaims)) string {
		claims := jwt.MapClaims{
			"sub":         "federation:ci",
			"permissions": []string{"instance:read"},
			"fed":         map[string]string{"issuer": "github", "rule": "ci", "rev": "rev1"},
		}
		alter(claims)
		return sign(claims)
	}

	_, err = verifier.ParseWithClaims(context.Background(), federated(func(jwt.MapClaims) {}), jwt.MapClaims{})
	require.NoError(t, err)
	_, err = verifier.ParseWithClaims(context.Background(), sign(jwt.MapClaims{"sub": "admin"}), jwt.MapClaims{})
	require.NoError(t, err, "tokens without a fed claim are unaffected")
	_, err = base.ParseWithClaims(context.Background(), sign(jwt.MapClaims{"sub": "admin"}), jwt.MapClaims{})
	require.NoError(t, err)

	for name, alter := range map[string]func(jwt.MapClaims){
		"changed rule":        func(c jwt.MapClaims) { c["fed"] = map[string]string{"issuer": "github", "rule": "ci", "rev": "rev0"} },
		"other issuer":        func(c jwt.MapClaims) { c["fed"] = map[string]string{"issuer": "other", "rule": "ci", "rev": "rev1"} },
		"removed rule":        func(c jwt.MapClaims) { c["fed"] = map[string]string{"issuer": "github", "rule": "gone", "rev": "rev1"} },
		"malformed lineage":   func(c jwt.MapClaims) { c["fed"] = "ci" },
		"empty lineage":       func(c jwt.MapClaims) { c["fed"] = map[string]string{} },
		"null lineage":        func(c jwt.MapClaims) { c["fed"] = nil },
		"another subject":     func(c jwt.MapClaims) { c["sub"] = "admin" },
		"no permissions":      func(c jwt.MapClaims) { delete(c, "permissions") },
		"null permissions":    func(c jwt.MapClaims) { c["permissions"] = nil },
		"invalid permissions": func(c jwt.MapClaims) { c["permissions"] = "instance:read" },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := verifier.ParseWithClaims(context.Background(), federated(alter), jwt.MapClaims{})
			require.ErrorIs(t, err, ErrFederationInactive)
		})
	}
	t.Run("no checker", func(t *testing.T) {
		_, err := base.ParseWithClaims(context.Background(), federated(func(jwt.MapClaims) {}), jwt.MapClaims{})
		require.ErrorIs(t, err, ErrFederationInactive)
	})
	t.Run("typed claims", func(t *testing.T) {
		_, err := verifier.ParseWithClaims(context.Background(), federated(func(c jwt.MapClaims) {
			c["fed"] = map[string]string{"issuer": "github", "rule": "ci", "rev": "rev0"}
		}), &jwt.RegisteredClaims{})
		require.ErrorIs(t, err, ErrFederationInactive, "the check does not depend on the caller's claims type")
	})
}

// Federated tokens are signed with the signing key like any other token, so
// they follow the same rotation phases and need no step of their own.
func TestFederatedTokensFollowKeyRotation(t *testing.T) {
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":         "federation:ci",
		"exp":         time.Now().Add(time.Hour).Unix(),
		"permissions": []string{"instance:read"},
		"fed":         map[string]string{"issuer": "github", "rule": "ci", "rev": "rev1"},
	}).SignedString([]byte("old"))
	require.NoError(t, err)
	for _, phase := range []struct {
		name     string
		primary  string
		fallback []string
		accepted bool
	}{
		{"accept both", "old", []string{"new"}, true},
		{"switched signing", "new", []string{"old"}, true},
		{"retired old", "new", nil, false},
	} {
		t.Run(phase.name, func(t *testing.T) {
			verifier, err := NewVerifier(phase.primary, phase.fallback, noop.NewMeterProvider().Meter("test"))
			require.NoError(t, err)
			_, err = verifier.WithFederation(federationRules{"ci": "github@rev1"}).ParseWithClaims(context.Background(), token, jwt.MapClaims{})
			if phase.accepted {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, jwt.ErrTokenSignatureInvalid)
			}
		})
	}
}
