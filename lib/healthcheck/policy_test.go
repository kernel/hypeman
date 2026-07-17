package healthcheck

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizePolicyInfersHTTPDefaults(t *testing.T) {
	policy, err := NormalizePolicy(&Policy{
		HTTP: &HTTPCheck{Port: 8080},
	})
	require.NoError(t, err)

	assert.Equal(t, TypeHTTP, policy.Type)
	assert.Equal(t, "10s", policy.Interval)
	assert.Equal(t, "2s", policy.Timeout)
	assert.Equal(t, "30s", policy.StartPeriod)
	assert.Equal(t, 3, policy.FailureThreshold)
	assert.Equal(t, 1, policy.SuccessThreshold)
	assert.Equal(t, "/", policy.HTTP.Path)
	assert.Equal(t, "http", policy.HTTP.Scheme)
	assert.Equal(t, 200, policy.HTTP.ExpectedStatus)
}

func TestNormalizePolicyRejectsExecWithoutCommand(t *testing.T) {
	_, err := NormalizePolicy(&Policy{
		Type: TypeExec,
		Exec: &ExecCheck{},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exec.command")
}

func TestNormalizePolicyRejectsAmbiguousChecks(t *testing.T) {
	_, err := NormalizePolicy(&Policy{
		HTTP: &HTTPCheck{Port: 8080},
		TCP:  &TCPCheck{Port: 8080},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one")
}

func TestNormalizePolicyRejectsMismatchedChecks(t *testing.T) {
	_, err := NormalizePolicy(&Policy{
		Type: TypeHTTP,
		HTTP: &HTTPCheck{Port: 8080},
		Exec: &ExecCheck{Command: []string{"true"}},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot include")
}

func TestNormalizePolicyClearsDisabledChecks(t *testing.T) {
	policy, err := NormalizePolicy(&Policy{
		Type: TypeNone,
		HTTP: &HTTPCheck{Port: 8080},
	})
	require.NoError(t, err)

	assert.Equal(t, TypeNone, policy.Type)
	assert.Nil(t, policy.HTTP)
}

func TestNormalizePolicyRejectsTimeoutLongerThanInterval(t *testing.T) {
	_, err := NormalizePolicy(&Policy{
		Type:     TypeTCP,
		Interval: "1s",
		Timeout:  "2s",
		TCP:      &TCPCheck{Port: 443},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeout")
}
