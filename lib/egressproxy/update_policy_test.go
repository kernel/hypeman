package egressproxy

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpdateInstancePolicy_ReplacesRules(t *testing.T) {
	t.Parallel()

	svc := &Service{
		policiesBySourceIP: map[string]sourcePolicy{
			"10.0.0.2": {
				headerInjectRules: []headerInjectRule{
					{headerName: "Authorization", headerValue: "Bearer old-key"},
				},
			},
		},
		sourceIPByInstance: map[string]string{
			"inst-1": "10.0.0.2",
		},
	}

	err := svc.UpdateInstancePolicy("inst-1", []HeaderInjectRuleConfig{
		{HeaderName: "Authorization", HeaderValue: "Bearer new-key", AllowedDomains: []string{"api.openai.com"}},
	})
	require.NoError(t, err)

	// Verify the new rules are applied
	hdr := http.Header{}
	svc.applyHeaderInjections("10.0.0.2", "api.openai.com", hdr, true)
	require.Equal(t, "Bearer new-key", hdr.Get("Authorization"))

	// Verify domain scoping works (should not inject for other domains)
	hdr2 := http.Header{}
	svc.applyHeaderInjections("10.0.0.2", "api.github.com", hdr2, true)
	require.Empty(t, hdr2.Get("Authorization"))
}

func TestUpdateInstancePolicy_ClearsRulesWithEmptySlice(t *testing.T) {
	t.Parallel()

	svc := &Service{
		policiesBySourceIP: map[string]sourcePolicy{
			"10.0.0.2": {
				headerInjectRules: []headerInjectRule{
					{headerName: "Authorization", headerValue: "Bearer old-key"},
				},
			},
		},
		sourceIPByInstance: map[string]string{
			"inst-1": "10.0.0.2",
		},
	}

	err := svc.UpdateInstancePolicy("inst-1", []HeaderInjectRuleConfig{})
	require.NoError(t, err)

	hdr := http.Header{}
	svc.applyHeaderInjections("10.0.0.2", "api.openai.com", hdr, true)
	require.Empty(t, hdr.Get("Authorization"))
}

func TestUpdateInstancePolicy_ErrorsForUnregisteredInstance(t *testing.T) {
	t.Parallel()

	svc := &Service{
		policiesBySourceIP: map[string]sourcePolicy{},
		sourceIPByInstance: map[string]string{},
	}

	err := svc.UpdateInstancePolicy("nonexistent", []HeaderInjectRuleConfig{
		{HeaderName: "Authorization", HeaderValue: "Bearer key"},
	})
	require.ErrorIs(t, err, ErrInstanceNotRegistered)
}

func TestUpdateInstancePolicy_IsIdempotent(t *testing.T) {
	t.Parallel()

	svc := &Service{
		policiesBySourceIP: map[string]sourcePolicy{
			"10.0.0.2": {},
		},
		sourceIPByInstance: map[string]string{
			"inst-1": "10.0.0.2",
		},
	}

	rules := []HeaderInjectRuleConfig{
		{HeaderName: "Authorization", HeaderValue: "Bearer same-key"},
	}

	// Call twice — should produce the same result
	require.NoError(t, svc.UpdateInstancePolicy("inst-1", rules))
	require.NoError(t, svc.UpdateInstancePolicy("inst-1", rules))

	hdr := http.Header{}
	svc.applyHeaderInjections("10.0.0.2", "api.example.com", hdr, true)
	require.Equal(t, "Bearer same-key", hdr.Get("Authorization"))
}

func TestUpdateInstancePolicy_DoesNotAffectOtherInstances(t *testing.T) {
	t.Parallel()

	matchers1, _ := compileDomainMatchers([]string{"api.openai.com"})
	svc := &Service{
		policiesBySourceIP: map[string]sourcePolicy{
			"10.0.0.2": {
				headerInjectRules: []headerInjectRule{
					{headerName: "Authorization", headerValue: "Bearer inst1-key", domainMatchers: matchers1},
				},
			},
			"10.0.0.3": {
				headerInjectRules: []headerInjectRule{
					{headerName: "Authorization", headerValue: "Bearer inst2-key"},
				},
			},
		},
		sourceIPByInstance: map[string]string{
			"inst-1": "10.0.0.2",
			"inst-2": "10.0.0.3",
		},
	}

	// Update inst-1 only
	err := svc.UpdateInstancePolicy("inst-1", []HeaderInjectRuleConfig{
		{HeaderName: "Authorization", HeaderValue: "Bearer inst1-new-key"},
	})
	require.NoError(t, err)

	// inst-2 should be unaffected
	hdr := http.Header{}
	svc.applyHeaderInjections("10.0.0.3", "api.example.com", hdr, true)
	require.Equal(t, "Bearer inst2-key", hdr.Get("Authorization"))
}

func TestUpdateInstancePolicy_RegisterThenUpdate(t *testing.T) {
	t.Parallel()

	// Simulate a service with a pre-registered instance (without starting a real listener)
	matchers, err := compileDomainMatchers([]string{"api.openai.com"})
	require.NoError(t, err)

	svc := &Service{
		policiesBySourceIP: map[string]sourcePolicy{
			"10.0.0.2": {
				headerInjectRules: []headerInjectRule{
					{headerName: "Authorization", headerValue: "Bearer original-key", domainMatchers: matchers},
				},
			},
		},
		sourceIPByInstance: map[string]string{
			"inst-1": "10.0.0.2",
		},
	}

	// Verify original key
	hdr := http.Header{}
	svc.applyHeaderInjections("10.0.0.2", "api.openai.com", hdr, true)
	require.Equal(t, "Bearer original-key", hdr.Get("Authorization"))

	// Update the policy with a rotated key
	err = svc.UpdateInstancePolicy("inst-1", []HeaderInjectRuleConfig{
		{HeaderName: "Authorization", HeaderValue: "Bearer rotated-key", AllowedDomains: []string{"api.openai.com"}},
	})
	require.NoError(t, err)

	// Verify rotated key is used
	hdr2 := http.Header{}
	svc.applyHeaderInjections("10.0.0.2", "api.openai.com", hdr2, true)
	require.Equal(t, "Bearer rotated-key", hdr2.Get("Authorization"))
}
