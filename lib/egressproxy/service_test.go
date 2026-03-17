package egressproxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeAllowedDomainPattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        string
		want      string
		shouldErr bool
	}{
		{name: "exact host", in: "API.OpenAI.com", want: "api.openai.com"},
		{name: "exact ip", in: "127.0.0.1", want: "127.0.0.1"},
		{name: "single wildcard", in: "*.OpenAI.com", want: "*.openai.com"},
		{name: "reject empty", in: "", shouldErr: true},
		{name: "reject scheme", in: "https://api.openai.com", shouldErr: true},
		{name: "reject port", in: "api.openai.com:443", shouldErr: true},
		{name: "reject global wildcard", in: "*", shouldErr: true},
		{name: "reject multi wildcard", in: "*.*.openai.com", shouldErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeAllowedDomainPattern(tt.in)
			if tt.shouldErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestDomainMatcherSingleLevelWildcard(t *testing.T) {
	t.Parallel()

	matchers, err := compileDomainMatchers([]string{"*.openai.com"})
	require.NoError(t, err)
	require.Len(t, matchers, 1)

	require.True(t, matchesAnyDomain("api.openai.com", matchers))
	require.False(t, matchesAnyDomain("openai.com", matchers))
	require.False(t, matchesAnyDomain("a.b.openai.com", matchers))
}

func TestApplyHeaderReplacementsHTTPSOnlyAndDomainGated(t *testing.T) {
	t.Parallel()

	matchers, err := compileDomainMatchers([]string{"api.openai.com"})
	require.NoError(t, err)

	svc := &Service{
		policiesBySourceIP: map[string]sourcePolicy{
			"10.0.0.2": {
				secretRewriteRules: []secretRewriteRule{
					{
						mockValue:      "mock-OUTBOUND_OPENAI_KEY",
						realValue:      "real-openai-key-123",
						domainMatchers: matchers,
					},
				},
			},
		},
	}

	httpsAllowed := http.Header{
		"Authorization": []string{"Bearer mock-OUTBOUND_OPENAI_KEY"},
	}
	svc.applyHeaderReplacements("10.0.0.2", "api.openai.com", httpsAllowed, true)
	require.Equal(t, "Bearer real-openai-key-123", httpsAllowed.Get("Authorization"))

	httpsBlocked := http.Header{
		"Authorization": []string{"Bearer mock-OUTBOUND_OPENAI_KEY"},
	}
	svc.applyHeaderReplacements("10.0.0.2", "api.github.com", httpsBlocked, true)
	require.Equal(t, "Bearer mock-OUTBOUND_OPENAI_KEY", httpsBlocked.Get("Authorization"))

	httpAllowedDomain := http.Header{
		"Authorization": []string{"Bearer mock-OUTBOUND_OPENAI_KEY"},
	}
	svc.applyHeaderReplacements("10.0.0.2", "api.openai.com", httpAllowedDomain, false)
	require.Equal(t, "Bearer mock-OUTBOUND_OPENAI_KEY", httpAllowedDomain.Get("Authorization"))
}

func TestHandleHTTPProxyRequest_DoesNotLeakUpstreamErrorDetails(t *testing.T) {
	t.Parallel()

	sentinelErr := errors.New("dial failed: test internal network detail")
	svc := &Service{
		transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, sentinelErr
			},
		},
		policiesBySourceIP: map[string]sourcePolicy{},
		sourceIPByInstance: map[string]string{},
	}

	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	svc.ServeHTTP(rec, req)

	resp := rec.Result()
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
	body := rec.Body.String()
	require.Contains(t, body, "proxy upstream error")
	require.NotContains(t, body, sentinelErr.Error())
	require.NotContains(t, body, "dial failed")
	require.False(t, strings.Contains(body, "internal network detail"))
}
