package healthcheck

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyProbeResultKeepsFailuresInStartPeriodStarting(t *testing.T) {
	startedAt := time.Date(2026, 5, 16, 1, 0, 0, 0, time.UTC)
	now := startedAt.Add(5 * time.Second)
	policy, err := NormalizePolicy(&Policy{
		Type:             TypeTCP,
		StartPeriod:      "30s",
		FailureThreshold: 1,
		TCP:              &TCPCheck{Port: 8080},
	})
	require.NoError(t, err)

	runtime := ApplyProbeResult(policy, Instance{
		State:     StateRunning,
		StartedAt: &startedAt,
	}, nil, now, ProbeResult{Success: false, Error: "connection refused"})

	assert.Equal(t, StatusStarting, runtime.Status)
	assert.Equal(t, 1, runtime.ConsecutiveFailures)
	assert.Equal(t, "connection refused", runtime.LastError)
}

func TestApplyProbeResultMarksUnhealthyAfterThreshold(t *testing.T) {
	startedAt := time.Date(2026, 5, 16, 1, 0, 0, 0, time.UTC)
	now := startedAt.Add(time.Minute)
	policy, err := NormalizePolicy(&Policy{
		Type:             TypeTCP,
		StartPeriod:      "10s",
		FailureThreshold: 2,
		TCP:              &TCPCheck{Port: 8080},
	})
	require.NoError(t, err)

	runtime := ApplyProbeResult(policy, Instance{State: StateRunning, StartedAt: &startedAt}, nil, now, ProbeResult{Success: false, Error: "first"})
	assert.Equal(t, StatusStarting, runtime.Status)

	runtime = ApplyProbeResult(policy, Instance{State: StateRunning, StartedAt: &startedAt}, runtime, now.Add(10*time.Second), ProbeResult{Success: false, Error: "second"})
	assert.Equal(t, StatusUnhealthy, runtime.Status)
	assert.Equal(t, 2, runtime.ConsecutiveFailures)
}

func TestApplyProbeResultMarksHealthyOnSuccess(t *testing.T) {
	now := time.Date(2026, 5, 16, 1, 0, 0, 0, time.UTC)
	policy, err := NormalizePolicy(&Policy{
		Type: TypeExec,
		Exec: &ExecCheck{Command: []string{"true"}},
	})
	require.NoError(t, err)

	runtime := ApplyProbeResult(policy, Instance{State: StateRunning}, nil, now, ProbeResult{Success: true})

	assert.Equal(t, StatusHealthy, runtime.Status)
	assert.Equal(t, 1, runtime.ConsecutiveSuccesses)
	assert.Zero(t, runtime.ConsecutiveFailures)
	assert.Empty(t, runtime.LastError)
}
