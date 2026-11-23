package oidcfederation

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"
)

const testSigningSecret = "test-signing-secret"

type exchangeFixture struct {
	assertionFixture
	exchanger *Exchanger
	rule      Rule
}

func newExchangeFixture(t *testing.T, cfg Config) *exchangeFixture {
	t.Helper()
	f := &exchangeFixture{assertionFixture: newAssertionFixture(t)}
	registry, err := Compile(cfg)
	require.NoError(t, err)
	f.exchanger, err = NewExchanger(registry, testSigningSecret, noop.NewMeterProvider().Meter("test"))
	require.NoError(t, err)
	f.exchanger.now = func() time.Time { return f.now }
	f.exchanger.startedAt = f.now.Add(-time.Hour)
	f.exchanger.verifier.now = f.exchanger.now
	f.exchanger.verifier.fetch = func(context.Context, IssuerSpec) ([]byte, error) { return f.keys, nil }
	f.rule, _ = registry.Rule("ci")
	return f
}

// assertion returns a signed GitHub assertion the fixture's rule accepts.
func (f *exchangeFixture) assertion(t *testing.T, alter func(jwt.MapClaims)) string {
	t.Helper()
	claims := f.claims()
	for name, value := range githubClaims() {
		claims[name] = value
	}
	if alter != nil {
		alter(claims)
	}
	return f.sign(t, claims, nil)
}

func TestExchangeIssuesAScopedShortLivedToken(t *testing.T) {
	f := newExchangeFixture(t, githubConfig())
	token, err := f.exchanger.Exchange(context.Background(), "ci", f.assertion(t, nil))
	require.NoError(t, err)
	require.Equal(t, DefaultTokenTTL, token.ExpiresIn)
	require.Equal(t, []string{"instance:read", "instance:write"}, token.Permissions)
	require.Equal(t, "federation:ci", token.Principal)
	require.Equal(t, "ci", token.Rule)

	claims := jwt.MapClaims{}
	_, err = jwt.ParseWithClaims(token.AccessToken, claims, func(*jwt.Token) (any, error) { return []byte(testSigningSecret), nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithTimeFunc(f.exchanger.now))
	require.NoError(t, err)
	require.Equal(t, "federation:ci", claims["sub"])
	require.EqualValues(t, f.now.Unix(), claims["iat"])
	require.EqualValues(t, f.now.Add(DefaultTokenTTL*time.Second).Unix(), claims["exp"])
	require.Len(t, claims["jti"], 32)
	require.Equal(t, []any{"instance:read", "instance:write"}, claims["permissions"])
	require.Equal(t, map[string]any{"issuer": "github-actions", "rule": "ci", "rev": f.rule.Revision}, claims["fed"])
	require.True(t, f.exchanger.Current("github-actions", "ci", f.rule.Revision))
	require.False(t, f.exchanger.Current("github-actions", "ci", "0000000000000000"))
	require.False(t, f.exchanger.Current("other", "ci", f.rule.Revision))
	require.False(t, f.exchanger.Current("github-actions", "gone", f.rule.Revision))
}

func TestExchangeClipsTheTokenToTheAssertion(t *testing.T) {
	f := newExchangeFixture(t, githubConfig())
	token, err := f.exchanger.Exchange(context.Background(), "ci", f.assertion(t, func(c jwt.MapClaims) {
		c["exp"] = f.now.Add(40 * time.Second).Unix()
	}))
	require.NoError(t, err)
	require.Equal(t, 40, token.ExpiresIn)
}

func TestExchangeRefusals(t *testing.T) {
	f := newExchangeFixture(t, githubConfig())
	for name, test := range map[string]struct {
		rule  string
		alter func(jwt.MapClaims)
	}{
		"unknown rule":     {"other", nil},
		"empty rule":       {"", nil},
		"another audience": {"ci", func(c jwt.MapClaims) { c["aud"] = "https://other.example.test" }},
		"another repo":     {"ci", func(c jwt.MapClaims) { c["repository_id"] = "99" }},
		"another branch":   {"ci", func(c jwt.MapClaims) { c["ref"] = "refs/heads/feature" }},
		"another event":    {"ci", func(c jwt.MapClaims) { c["event_name"] = "pull_request" }},
		"expired":          {"ci", func(c jwt.MapClaims) { c["exp"] = f.now.Add(-time.Minute).Unix() }},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.exchanger.Exchange(context.Background(), test.rule, f.assertion(t, test.alter))
			require.ErrorIs(t, err, ErrAssertion)
		})
	}
	_, err := f.exchanger.Exchange(context.Background(), "ci", "not-a-jwt")
	require.ErrorIs(t, err, ErrAssertion)
}

func TestExchangeSpendsASingleUseAssertionOnce(t *testing.T) {
	f := newExchangeFixture(t, githubConfig())
	assertion := f.assertion(t, nil)
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for range cap(results) {
		wg.Go(func() {
			_, err := f.exchanger.Exchange(context.Background(), "ci", assertion)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	issued := 0
	for err := range results {
		if err == nil {
			issued++
		} else {
			require.ErrorIs(t, err, ErrAssertion)
		}
	}
	require.Equal(t, 1, issued)

	// A refused exchange leaves the assertion usable.
	refused := f.assertion(t, func(c jwt.MapClaims) { c["jti"] = "kept"; c["ref"] = "refs/heads/feature" })
	_, err := f.exchanger.Exchange(context.Background(), "ci", refused)
	require.ErrorIs(t, err, ErrAssertion)
	require.Equal(t, spentRecorded, f.exchanger.consume("github-actions", "kept", f.now.Add(time.Minute), f.now))
}

// The record of spent assertions is lost on restart. An issuer whose clock runs
// ahead stamps an iat later than the restart, so comparing iat with the start
// time alone would let a spent assertion through.
func TestRestartDoesNotReopenASpentAssertion(t *testing.T) {
	first := newExchangeFixture(t, githubConfig())
	start := first.now
	spent := first.assertion(t, func(c jwt.MapClaims) { c["iat"] = start.Add(20 * time.Second).Unix() })
	_, err := first.exchanger.Exchange(context.Background(), "ci", spent)
	require.NoError(t, err, "the verifier allows for a fast issuer clock")

	second := newExchangeFixture(t, githubConfig())
	second.keys, second.key = first.keys, first.key
	second.exchanger.startedAt = start.Add(5 * time.Second)
	floor := second.exchanger.startedAt.Add(clockLeeway)
	// issued returns an assertion stamped by an issuer whose clock differs from the host's.
	issued := func(skew time.Duration) string {
		return second.assertion(t, func(c jwt.MapClaims) {
			c["iat"], c["exp"] = second.now.Add(skew).Unix(), second.now.Add(skew+5*time.Minute).Unix()
		})
	}
	exchange := func(assertion string) error {
		_, err := second.exchanger.Exchange(context.Background(), "ci", assertion)
		return err
	}

	second.now = start.Add(10 * time.Second)
	require.ErrorIs(t, exchange(spent), ErrUnavailable)
	require.ErrorIs(t, exchange(issued(0)), ErrUnavailable, "a fresh assertion is below the floor right after a start")
	require.NoError(t, exchange(issued(25*time.Second)), "an assertion at or above the floor could not have been spent before")

	second.now = floor
	require.NoError(t, exchange(issued(0)))
	require.ErrorIs(t, exchange(spent), ErrUnavailable)
	require.ErrorIs(t, exchange(issued(-20*time.Second)), ErrUnavailable, "a slow issuer needs longer, and is told to retry")

	second.now = floor.Add(20 * time.Second)
	require.NoError(t, exchange(issued(-20*time.Second)))

	second.now = floor.Add(clockLeeway + time.Second)
	require.ErrorIs(t, exchange(spent), ErrAssertion, "an assertion the earlier process could have spent stays refused")
	require.NoError(t, exchange(issued(-clockLeeway)), "by then an issuer within the leeway is above the floor")
}

// expires_in is the lifetime the token states, whatever fraction of a second
// the exchange happens at.
func TestExchangeReportsTheTokensOwnLifetime(t *testing.T) {
	for _, ttl := range []int{1, DefaultTokenTTL} {
		cfg := githubConfig()
		cfg.FederationRules[0].Authorization.MaxTokenTTLSeconds = &ttl
		f := newExchangeFixture(t, cfg)
		f.exchanger.now = func() time.Time { return f.now.Add(700 * time.Millisecond) }
		f.exchanger.verifier.now = f.exchanger.now
		token, err := f.exchanger.Exchange(context.Background(), "ci", f.assertion(t, nil))
		require.NoError(t, err)
		require.Equal(t, ttl, token.ExpiresIn)
		claims := jwt.MapClaims{}
		_, _, err = jwt.NewParser().ParseUnverified(token.AccessToken, claims)
		require.NoError(t, err)
		require.EqualValues(t, f.now.Unix(), claims["iat"])
		require.EqualValues(t, f.now.Unix()+int64(ttl), claims["exp"])
	}
}

func TestExchangeAllowsReuseWhereTheIssuerDoes(t *testing.T) {
	f := newExchangeFixture(t, Config{
		Issuers: []IssuerSpec{{Name: "cluster", IssuerURL: "https://cluster.example.test", PresetType: "kubernetes"}},
		FederationRules: []RuleSpec{{
			Name: "ci", Issuer: "cluster", ExpectedAudience: "https://auth.example.test",
			Match:         Match{Type: "kubernetes", Namespace: "ci", ServiceAccount: "runner"},
			Authorization: RuleAuthorization{Permissions: []string{"image:read"}},
		}},
	})
	f.exchanger.startedAt = f.now.Add(time.Hour)
	claims := f.claims()
	claims["iss"], claims["sub"] = "https://cluster.example.test", "system:serviceaccount:ci:runner"
	assertion := f.sign(t, claims, nil)
	for range 2 {
		_, err := f.exchanger.Exchange(context.Background(), "ci", assertion)
		require.NoError(t, err)
	}
}

func TestExchangeReportsUnavailableKeys(t *testing.T) {
	f := newExchangeFixture(t, githubConfig())
	f.exchanger.verifier.fetch = func(context.Context, IssuerSpec) ([]byte, error) {
		return nil, &KeyProblem{"the key endpoint returned HTTP 404"}
	}
	_, err := f.exchanger.Exchange(context.Background(), "ci", f.assertion(t, nil))
	require.ErrorIs(t, err, ErrUnavailable)
	require.NotErrorIs(t, err, ErrAssertion)
}

func TestSpentAssertionsAreBoundedAndExpire(t *testing.T) {
	f := newExchangeFixture(t, githubConfig())
	e := f.exchanger
	expires := f.now.Add(time.Minute)
	require.Equal(t, spentRecorded, e.consume("a", "one", expires, f.now))
	require.Equal(t, spentAlready, e.consume("a", "one", expires, f.now))
	require.Equal(t, spentRecorded, e.consume("b", "one", expires, f.now), "uniqueness is per issuer")
	require.Equal(t, spentAlready, e.consume("a", "one", expires, expires.Add(clockLeeway-time.Second)), "held for as long as the verifier accepts it")

	for i := 0; len(e.spent) < maxSpentAssertions; i++ {
		e.spent[fmt.Sprintf("filler\x00%d", i)] = expires
	}
	require.Equal(t, spentFull, e.consume("a", "two", expires, f.now))
	later := expires.Add(clockLeeway)
	require.Equal(t, spentRecorded, e.consume("a", "two", later.Add(time.Minute), later), "expired records are swept")
	require.Len(t, e.spent, 1)
}

func postToken(e *Exchanger, body string, alter func(*http.Request)) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if alter != nil {
		alter(request)
	}
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	return recorder
}

func tokenForm(assertion string) url.Values {
	return url.Values{"grant_type": {GrantTypeJWTBearer}, "assertion": {assertion}, "federation_rule_id": {"ci"}}
}

func TestTokenEndpointIssuesAToken(t *testing.T) {
	f := newExchangeFixture(t, githubConfig())
	recorder := postToken(f.exchanger, tokenForm(f.assertion(t, nil)).Encode(), nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	require.Equal(t, "application/json", recorder.Header().Get("Content-Type"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.NotEmpty(t, body["access_token"])
	delete(body, "access_token")
	require.Equal(t, map[string]any{
		"token_type": "Bearer", "expires_in": float64(DefaultTokenTTL), "scope": "instance:read instance:write",
		"principal": "federation:ci", "federation_rule_id": "ci",
	}, body)
}

func TestTokenEndpointErrors(t *testing.T) {
	f := newExchangeFixture(t, githubConfig())
	valid := func() url.Values { return tokenForm(f.assertion(t, nil)) }
	with := func(alter func(url.Values)) string {
		form := valid()
		alter(form)
		return form.Encode()
	}
	for name, test := range map[string]struct {
		body   string
		alter  func(*http.Request)
		status int
		code   string
	}{
		"no grant type":      {with(func(v url.Values) { v.Del("grant_type") }), nil, 400, "invalid_request"},
		"other grant type":   {with(func(v url.Values) { v.Set("grant_type", "client_credentials") }), nil, 400, "unsupported_grant_type"},
		"no assertion":       {with(func(v url.Values) { v.Del("assertion") }), nil, 400, "invalid_request"},
		"no rule":            {with(func(v url.Values) { v.Del("federation_rule_id") }), nil, 400, "invalid_request"},
		"unknown parameter":  {with(func(v url.Values) { v.Set("client_secret", "s") }), nil, 400, "invalid_request"},
		"repeated parameter": {with(func(v url.Values) { v.Add("federation_rule_id", "other") }), nil, 400, "invalid_request"},
		"repeated grant":     {with(func(v url.Values) { v.Add("grant_type", GrantTypeJWTBearer) }), nil, 400, "invalid_request"},
		"malformed body":     {"grant_type=%zz", nil, 400, "invalid_request"},
		"oversized body":     {"grant_type=" + strings.Repeat("a", maxTokenRequestBytes), nil, 400, "invalid_request"},
		"json body":          {`{"grant_type":"x"}`, func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }, 400, "invalid_request"},
		"client credentials": {valid().Encode(), func(r *http.Request) { r.Header.Set("Authorization", "Bearer token") }, 400, "invalid_request"},
		"empty credentials":  {valid().Encode(), func(r *http.Request) { r.Header["Authorization"] = []string{""} }, 400, "invalid_request"},
		"query parameters":   {with(func(v url.Values) { v.Del("assertion") }), func(r *http.Request) { r.URL.RawQuery = "assertion=in-the-url" }, 400, "invalid_request"},
		"unknown rule":       {with(func(v url.Values) { v.Set("federation_rule_id", "other") }), nil, 400, "invalid_grant"},
		"bad assertion":      {with(func(v url.Values) { v.Set("assertion", "not-a-jwt") }), nil, 400, "invalid_grant"},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := postToken(f.exchanger, test.body, test.alter)
			require.Equal(t, test.status, recorder.Code, recorder.Body.String())
			require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
			var body tokenError
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
			require.Equal(t, test.code, body.Error)
			require.NotEmpty(t, body.ErrorDescription)
		})
	}

	// Every refusal of a rule or an assertion reads the same.
	unknown := postToken(f.exchanger, with(func(v url.Values) { v.Set("federation_rule_id", "other") }), nil)
	mismatch := postToken(f.exchanger, tokenForm(f.assertion(t, func(c jwt.MapClaims) { c["repository_id"] = "99" })).Encode(), nil)
	require.Equal(t, unknown.Body.String(), mismatch.Body.String())

	f.exchanger.verifier.fetch = func(context.Context, IssuerSpec) ([]byte, error) { return nil, ErrUnavailable }
	f.exchanger.verifier.cache = map[string]cachedKeys{}
	unavailable := postToken(f.exchanger, valid().Encode(), nil)
	require.Equal(t, http.StatusServiceUnavailable, unavailable.Code)
	require.Contains(t, unavailable.Body.String(), "temporarily_unavailable")
}

// unwrappingWriter stands in for the server's access-log writer, which hides
// the connection from everything but http.ResponseController.
type unwrappingWriter struct{ http.ResponseWriter }

func (w unwrappingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// A caller that stops sending must not hold the connection: not while the form
// is read, and not while the server discards the rest of a rejected request.
func TestTokenEndpointDoesNotWaitForAStalledBody(t *testing.T) {
	f := newExchangeFixture(t, githubConfig())
	f.exchanger.readTimeout = 200 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.exchanger.ServeHTTP(unwrappingWriter{w}, r)
	}))
	defer server.Close()
	request := func(contentType string, contentLength int, sent string) string {
		return fmt.Sprintf("POST /oauth/token HTTP/1.1\r\nHost: hypeman\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n%s", contentType, contentLength, sent)
	}
	for name, raw := range map[string]string{
		"rejected before the body is read": request("application/json", 4096, ""),
		"form that never arrives":          request("application/x-www-form-urlencoded", 4096, "grant_type="),
		"form over the size limit":         request("application/x-www-form-urlencoded", 2*maxTokenRequestBytes, "assertion="+strings.Repeat("a", maxTokenRequestBytes)),
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := net.Dial("tcp", server.Listener.Addr().String())
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
			_, err = conn.Write([]byte(raw))
			require.NoError(t, err)
			reader := bufio.NewReader(conn)
			response, err := http.ReadResponse(reader, nil)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusBadRequest, response.StatusCode)
			require.Contains(t, string(body), "invalid_request")
			_, err = io.Copy(io.Discard, reader)
			require.NoError(t, err, "the server closes the connection instead of waiting for the rest")
		})
	}
}

func TestTokenEndpointWithoutFederation(t *testing.T) {
	registry, err := Compile(Config{})
	require.NoError(t, err)
	exchanger, err := NewExchanger(registry, testSigningSecret, noop.NewMeterProvider().Meter("test"))
	require.NoError(t, err)
	recorder := postToken(exchanger, tokenForm("assertion").Encode(), nil)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "unsupported_grant_type")
	require.False(t, exchanger.Current("github-actions", "ci", "rev"))

	_, err = NewExchanger(registry, "", noop.NewMeterProvider().Meter("test"))
	require.Error(t, err)
}
