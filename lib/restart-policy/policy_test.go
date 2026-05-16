package restartpolicy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizePolicyDefaults(t *testing.T) {
	policy, err := NormalizePolicy(&Policy{Policy: PolicyOnFailure})
	require.NoError(t, err)

	assert.Equal(t, PolicyOnFailure, policy.Policy)
	assert.Equal(t, "5s", policy.Backoff)
	assert.Equal(t, "10m0s", policy.StableAfter)
	assert.Equal(t, 0, policy.MaxAttempts)
}

func TestNormalizePolicyNeverBecomesNil(t *testing.T) {
	policy, err := NormalizePolicy(&Policy{Policy: PolicyNever})
	require.NoError(t, err)

	assert.Nil(t, policy)
}

func TestNormalizePolicyRejectsInvalidDuration(t *testing.T) {
	_, err := NormalizePolicy(&Policy{Policy: PolicyAlways, Backoff: "0s"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "restart_policy.backoff")
}

func TestShouldRestart(t *testing.T) {
	exitZero := 0
	exitOne := 1

	assert.False(t, ShouldRestart(nil, &exitOne))
	assert.True(t, ShouldRestart(&Policy{Policy: PolicyAlways}, &exitZero))
	assert.False(t, ShouldRestart(&Policy{Policy: PolicyOnFailure}, &exitZero))
	assert.True(t, ShouldRestart(&Policy{Policy: PolicyOnFailure}, &exitOne))
	assert.True(t, ShouldRestart(&Policy{Policy: PolicyOnFailure}, nil))
}

func TestStableAttemptReset(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-11 * time.Minute)

	reset := shouldResetStableAttempts(
		&Policy{Policy: PolicyAlways, StableAfter: "10m"},
		Status{Attempts: 2},
		Instance{State: StateRunning, StartedAt: &startedAt},
		now,
	)

	assert.True(t, reset)
}
