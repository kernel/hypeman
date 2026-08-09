package api

import (
	"encoding/json"
	"testing"

	"github.com/kernel/hypeman/lib/ingress"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIngressRequestHeaderAuthAPIRoundTrip(t *testing.T) {
	t.Setenv("HYPEMAN_INGRESS_AUTH_SERVICE", "secret-value")
	port := 443
	tls := true
	redirect := true
	apiRule := oapi.IngressRule{
		Match:        oapi.IngressMatch{Hostname: "service.example.com", Port: &port},
		Target:       oapi.IngressTarget{Instance: "service", Port: 8080},
		Tls:          &tls,
		RedirectHttp: &redirect,
		RequestHeaderAuth: &oapi.IngressRequestHeaderAuth{
			Header:    "X-Ingress-Verification",
			SecretEnv: "HYPEMAN_INGRESS_AUTH_SERVICE",
		},
	}

	domainRule := ingressRuleFromOAPI(apiRule)
	require.NotNil(t, domainRule.RequestHeaderAuth)
	assert.Equal(t, "X-Ingress-Verification", domainRule.RequestHeaderAuth.Header)
	assert.Equal(t, "HYPEMAN_INGRESS_AUTH_SERVICE", domainRule.RequestHeaderAuth.SecretEnv)

	roundTrip := ingressRuleToOAPI(domainRule)
	assert.Equal(t, apiRule, roundTrip)
	encoded, err := json.Marshal(roundTrip)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), "HYPEMAN_INGRESS_AUTH_SERVICE")
	assert.NotContains(t, string(encoded), "secret-value")
}

func TestIngressRequestHeaderAuthOmittedWhenUnprotected(t *testing.T) {
	rule := ingressRuleToOAPI(ingress.IngressRule{
		Match:  ingress.IngressMatch{Hostname: "service.example.com", Port: 80},
		Target: ingress.IngressTarget{Instance: "service", Port: 8080},
	})
	assert.Nil(t, rule.RequestHeaderAuth)
}
