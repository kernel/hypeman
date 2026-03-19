package egressproxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	svc, err := NewService(t.TempDir(), 0)
	require.NoError(t, err)
	return svc
}

func TestUpdateInstancePolicy_NotRegistered(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	updated, err := svc.UpdateInstancePolicy("unknown-id", []HeaderInjectRuleConfig{
		{HeaderName: "Authorization", HeaderValue: "Bearer new-key", AllowedDomains: []string{"api.example.com"}},
	})
	require.NoError(t, err)
	assert.False(t, updated, "should return false for unregistered instance")
}

func TestUpdateInstancePolicy_UpdatesExisting(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	// Manually register an instance (bypass RegisterInstance which needs networking)
	svc.mu.Lock()
	svc.sourceIPByInstance["inst-1"] = "10.0.0.2"
	svc.policiesBySourceIP["10.0.0.2"] = sourcePolicy{
		headerInjectRules: []headerInjectRule{
			{headerName: "Authorization", headerValue: "Bearer old-key"},
		},
	}
	svc.mu.Unlock()

	// Update the policy
	updated, err := svc.UpdateInstancePolicy("inst-1", []HeaderInjectRuleConfig{
		{HeaderName: "Authorization", HeaderValue: "Bearer rotated-key", AllowedDomains: []string{"api.example.com"}},
	})
	require.NoError(t, err)
	assert.True(t, updated)

	// Verify the policy was updated
	svc.mu.Lock()
	policy := svc.policiesBySourceIP["10.0.0.2"]
	svc.mu.Unlock()

	require.Len(t, policy.headerInjectRules, 1)
	assert.Equal(t, "Authorization", policy.headerInjectRules[0].headerName)
	assert.Equal(t, "Bearer rotated-key", policy.headerInjectRules[0].headerValue)
}

func TestUpdateInstancePolicy_ClearsRulesWithEmptySlice(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	// Register instance with a rule
	svc.mu.Lock()
	svc.sourceIPByInstance["inst-2"] = "10.0.0.3"
	svc.policiesBySourceIP["10.0.0.3"] = sourcePolicy{
		headerInjectRules: []headerInjectRule{
			{headerName: "Authorization", headerValue: "Bearer old-key"},
		},
	}
	svc.mu.Unlock()

	// Update with empty rules
	updated, err := svc.UpdateInstancePolicy("inst-2", nil)
	require.NoError(t, err)
	assert.True(t, updated)

	svc.mu.Lock()
	policy := svc.policiesBySourceIP["10.0.0.3"]
	svc.mu.Unlock()
	assert.Empty(t, policy.headerInjectRules)
}
