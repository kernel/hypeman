package instances

import (
	"errors"
	"testing"
	"time"

	restartpolicy "github.com/kernel/hypeman/lib/restart-policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateUpdateInstanceRequestAllowsRestartPolicyOnly(t *testing.T) {
	err := validateUpdateInstanceRequest(&metadata{}, UpdateInstanceRequest{
		RestartPolicy:    &restartpolicy.Policy{Policy: restartpolicy.PolicyAlways},
		RestartPolicySet: true,
	})

	require.NoError(t, err)
}

func TestNormalizeRestartPolicyWrapsInvalidRequest(t *testing.T) {
	_, err := normalizeRestartPolicy(&restartpolicy.Policy{
		Policy:  restartpolicy.PolicyOnFailure,
		Backoff: "0s",
	})

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidRequest))
}

func TestRestartStatusAfterPolicyUpdatePreservesManualStop(t *testing.T) {
	status := restartStatusAfterPolicyUpdate(restartpolicy.Status{
		Attempts:      3,
		BlockedReason: restartpolicy.BlockedReasonManualStop,
	})

	assert.Equal(t, restartpolicy.BlockedReasonManualStop, status.BlockedReason)
	assert.Zero(t, status.Attempts)
}

func TestRestartStatusAfterPolicyUpdateClearsRetryState(t *testing.T) {
	status := restartStatusAfterPolicyUpdate(restartpolicy.Status{
		Attempts:      3,
		BlockedReason: restartpolicy.BlockedReasonMaxAttemptsExceeded,
	})

	assert.True(t, status.IsZero())
}

func TestShouldResetRestartAttempts(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-11 * time.Minute)

	reset := shouldResetRestartAttempts(
		&restartpolicy.Policy{Policy: restartpolicy.PolicyAlways, StableAfter: "10m"},
		restartpolicy.Status{Attempts: 2},
		&Instance{
			State:          StateRunning,
			StoredMetadata: StoredMetadata{StartedAt: &startedAt},
		},
		now,
	)

	assert.True(t, reset)
}
