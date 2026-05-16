package restartpolicy

import (
	"fmt"
	"strings"
	"time"
)

const (
	DefaultBackoff     = 5 * time.Second
	DefaultStableAfter = 10 * time.Minute
)

type PolicyMode string

const (
	PolicyNever     PolicyMode = "never"
	PolicyAlways    PolicyMode = "always"
	PolicyOnFailure PolicyMode = "on_failure"
)

type BlockedReason string

const (
	BlockedReasonManualStop          BlockedReason = "manual_stop"
	BlockedReasonMaxAttemptsExceeded BlockedReason = "max_attempts_exceeded"
)

type RestartReason string

const (
	RestartReasonHealthCheckFailed RestartReason = "health_check_failed"
)

type Policy struct {
	Policy      PolicyMode `json:"policy"`
	Backoff     string     `json:"backoff,omitempty"`
	MaxAttempts int        `json:"max_attempts,omitempty"`
	StableAfter string     `json:"stable_after,omitempty"`
}

type Status struct {
	Attempts      int           `json:"attempts,omitempty"`
	LastAttemptAt *time.Time    `json:"last_attempt_at,omitempty"`
	NextAttemptAt *time.Time    `json:"next_attempt_at,omitempty"`
	BlockedReason BlockedReason `json:"blocked_reason,omitempty"`
	LastReason    RestartReason `json:"last_reason,omitempty"`
}

func NormalizePolicy(policy *Policy) (*Policy, error) {
	if policy == nil {
		return nil, nil
	}

	mode := policy.Policy
	if mode == "" {
		mode = PolicyNever
	}

	switch mode {
	case PolicyNever:
		return nil, nil
	case PolicyAlways, PolicyOnFailure:
	default:
		return nil, fmt.Errorf("restart_policy.policy must be one of never, always, on_failure")
	}

	backoff, err := normalizeDuration(policy.Backoff, DefaultBackoff, "restart_policy.backoff")
	if err != nil {
		return nil, err
	}
	stableAfter, err := normalizeDuration(policy.StableAfter, DefaultStableAfter, "restart_policy.stable_after")
	if err != nil {
		return nil, err
	}
	if policy.MaxAttempts < 0 {
		return nil, fmt.Errorf("restart_policy.max_attempts must be >= 0")
	}

	return &Policy{
		Policy:      mode,
		Backoff:     backoff.String(),
		MaxAttempts: policy.MaxAttempts,
		StableAfter: stableAfter.String(),
	}, nil
}

func Backoff(policy *Policy) time.Duration {
	d, err := durationOrDefault(policy, func(p *Policy) string { return p.Backoff }, DefaultBackoff)
	if err != nil {
		return DefaultBackoff
	}
	return d
}

func StableAfter(policy *Policy) time.Duration {
	d, err := durationOrDefault(policy, func(p *Policy) string { return p.StableAfter }, DefaultStableAfter)
	if err != nil {
		return DefaultStableAfter
	}
	return d
}

func Failure(exitCode *int) bool {
	return exitCode == nil || *exitCode != 0
}

func ShouldRestart(policy *Policy, exitCode *int) bool {
	if policy == nil {
		return false
	}
	switch policy.Policy {
	case PolicyAlways:
		return true
	case PolicyOnFailure:
		return Failure(exitCode)
	default:
		return false
	}
}

func ShouldRestartHealthCheck(policy *Policy) bool {
	if policy == nil {
		return false
	}
	switch policy.Policy {
	case PolicyAlways, PolicyOnFailure:
		return true
	default:
		return false
	}
}

func ShouldRestartInstance(policy *Policy, exitCode *int, status Status) bool {
	if status.LastReason == RestartReasonHealthCheckFailed {
		return ShouldRestartHealthCheck(policy)
	}
	return ShouldRestart(policy, exitCode)
}

func PrepareAttempt(policy *Policy, status Status, now time.Time) (Status, bool) {
	now = now.UTC()
	if status.BlockedReason != "" {
		return status, false
	}
	if status.NextAttemptAt != nil && now.Before(status.NextAttemptAt.UTC()) {
		return status, false
	}
	if status.LastAttemptAt != nil {
		nextAttemptAt := status.LastAttemptAt.UTC().Add(Backoff(policy))
		if now.Before(nextAttemptAt) {
			status.NextAttemptAt = &nextAttemptAt
			return status, false
		}
	}
	if policy.MaxAttempts > 0 && status.Attempts >= policy.MaxAttempts {
		status.NextAttemptAt = nil
		status.BlockedReason = BlockedReasonMaxAttemptsExceeded
		return status, false
	}

	status.Attempts++
	status.LastAttemptAt = &now
	status.NextAttemptAt = nil
	return status, true
}

func AfterFailedAttempt(policy *Policy, status Status, now time.Time) Status {
	now = now.UTC()
	if policy.MaxAttempts > 0 && status.Attempts >= policy.MaxAttempts {
		status.BlockedReason = BlockedReasonMaxAttemptsExceeded
		status.NextAttemptAt = nil
		return status
	}
	nextAttemptAt := now.Add(Backoff(policy))
	status.NextAttemptAt = &nextAttemptAt
	return status
}

func EqualStatus(a, b Status) bool {
	return a.Attempts == b.Attempts &&
		equalTime(a.LastAttemptAt, b.LastAttemptAt) &&
		equalTime(a.NextAttemptAt, b.NextAttemptAt) &&
		a.BlockedReason == b.BlockedReason &&
		a.LastReason == b.LastReason
}

func (s Status) IsZero() bool {
	return s.Attempts == 0 &&
		s.LastAttemptAt == nil &&
		s.NextAttemptAt == nil &&
		s.BlockedReason == "" &&
		s.LastReason == ""
}

func equalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.UTC().Equal(b.UTC())
}

func normalizeDuration(raw string, fallback time.Duration, field string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid duration: %w", field, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be positive", field)
	}
	return parsed, nil
}

func durationOrDefault(policy *Policy, selectValue func(*Policy) string, fallback time.Duration) (time.Duration, error) {
	if policy == nil {
		return fallback, nil
	}
	return normalizeDuration(selectValue(policy), fallback, "duration")
}
