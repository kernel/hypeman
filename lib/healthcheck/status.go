package healthcheck

import "time"

const maxLastErrorLength = 512

type ProbeResult struct {
	Success bool
	Error   string
}

func Snapshot(policy *Policy, state string, runtime *Runtime) StatusSnapshot {
	if !Enabled(policy) {
		return StatusSnapshot{Status: StatusDisabled}
	}
	if state == StateInitializing {
		snapshot := snapshotRuntime(runtime)
		snapshot.Status = StatusStarting
		return snapshot
	}
	if state != StateRunning {
		return StatusSnapshot{Status: StatusUnknown}
	}
	if runtime == nil || runtime.Status == "" {
		return StatusSnapshot{Status: StatusStarting}
	}
	return snapshotRuntime(runtime)
}

func snapshotRuntime(runtime *Runtime) StatusSnapshot {
	if runtime == nil {
		return StatusSnapshot{}
	}
	return StatusSnapshot{
		Status:               runtime.Status,
		ConsecutiveSuccesses: runtime.ConsecutiveSuccesses,
		ConsecutiveFailures:  runtime.ConsecutiveFailures,
		LastCheckedAt:        cloneTime(runtime.LastCheckedAt),
		LastSuccessAt:        cloneTime(runtime.LastSuccessAt),
		LastFailureAt:        cloneTime(runtime.LastFailureAt),
		LastError:            runtime.LastError,
	}
}

func ApplyProbeResult(policy *Policy, inst Instance, previous *Runtime, now time.Time, result ProbeResult) *Runtime {
	runtime := CloneRuntime(previous)
	if runtime == nil {
		runtime = &Runtime{}
	}

	now = now.UTC()
	if runtime.StartedAt == nil {
		startedAt := now
		if inst.StartedAt != nil {
			startedAt = inst.StartedAt.UTC()
		}
		runtime.StartedAt = &startedAt
	}
	runtime.LastCheckedAt = &now

	_, _, startPeriod, err := DurationConfig(policy)
	if err != nil {
		startPeriod = defaultStartPeriod
	}
	inStartPeriod := runtime.StartedAt != nil && now.Sub(*runtime.StartedAt) < startPeriod

	if result.Success {
		runtime.ConsecutiveSuccesses++
		runtime.ConsecutiveFailures = 0
		runtime.LastSuccessAt = &now
		runtime.LastError = ""
		successThreshold := defaultSuccessThreshold
		if policy != nil {
			successThreshold = policy.SuccessThreshold
		}
		if successThreshold == 0 {
			successThreshold = defaultSuccessThreshold
		}
		if runtime.ConsecutiveSuccesses >= successThreshold {
			runtime.Status = StatusHealthy
		} else if runtime.Status == "" {
			runtime.Status = StatusStarting
		}
		return runtime
	}

	runtime.ConsecutiveFailures++
	runtime.ConsecutiveSuccesses = 0
	runtime.LastFailureAt = &now
	runtime.LastError = truncateLastError(result.Error)

	failureThreshold := defaultFailureThreshold
	if policy != nil {
		failureThreshold = policy.FailureThreshold
	}
	if failureThreshold == 0 {
		failureThreshold = defaultFailureThreshold
	}
	if !inStartPeriod && runtime.ConsecutiveFailures >= failureThreshold {
		runtime.Status = StatusUnhealthy
	} else if runtime.Status == "" {
		runtime.Status = StatusStarting
	}

	return runtime
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	cloned := t.UTC()
	return &cloned
}

func truncateLastError(value string) string {
	if len(value) <= maxLastErrorLength {
		return value
	}

	end := 0
	for i := range value {
		if i > maxLastErrorLength {
			break
		}
		end = i
	}
	return value[:end]
}
