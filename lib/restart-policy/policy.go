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

func (s Status) IsZero() bool {
	return s.Attempts == 0 &&
		s.LastAttemptAt == nil &&
		s.NextAttemptAt == nil &&
		s.BlockedReason == ""
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
