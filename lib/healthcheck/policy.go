package healthcheck

import (
	"fmt"
	"time"
)

const (
	defaultInterval         = 10 * time.Second
	defaultTimeout          = 2 * time.Second
	defaultStartPeriod      = 30 * time.Second
	defaultFailureThreshold = 3
	defaultSuccessThreshold = 1
)

func ClonePolicy(policy *Policy) *Policy {
	if policy == nil {
		return nil
	}

	cloned := &Policy{
		Type:             policy.Type,
		Interval:         policy.Interval,
		Timeout:          policy.Timeout,
		StartPeriod:      policy.StartPeriod,
		FailureThreshold: policy.FailureThreshold,
		SuccessThreshold: policy.SuccessThreshold,
	}
	if policy.HTTP != nil {
		cloned.HTTP = &HTTPCheck{
			Port:           policy.HTTP.Port,
			Path:           policy.HTTP.Path,
			Scheme:         policy.HTTP.Scheme,
			ExpectedStatus: policy.HTTP.ExpectedStatus,
		}
	}
	if policy.TCP != nil {
		cloned.TCP = &TCPCheck{Port: policy.TCP.Port}
	}
	if policy.Exec != nil {
		cloned.Exec = &ExecCheck{
			Command:    append([]string(nil), policy.Exec.Command...),
			WorkingDir: policy.Exec.WorkingDir,
		}
	}
	return cloned
}

func CloneRuntime(runtime *Runtime) *Runtime {
	if runtime == nil {
		return nil
	}

	cloned := &Runtime{
		Status:               runtime.Status,
		ConsecutiveSuccesses: runtime.ConsecutiveSuccesses,
		ConsecutiveFailures:  runtime.ConsecutiveFailures,
		LastError:            runtime.LastError,
	}
	if runtime.StartedAt != nil {
		startedAt := runtime.StartedAt.UTC()
		cloned.StartedAt = &startedAt
	}
	if runtime.LastCheckedAt != nil {
		lastCheckedAt := runtime.LastCheckedAt.UTC()
		cloned.LastCheckedAt = &lastCheckedAt
	}
	if runtime.LastSuccessAt != nil {
		lastSuccessAt := runtime.LastSuccessAt.UTC()
		cloned.LastSuccessAt = &lastSuccessAt
	}
	if runtime.LastFailureAt != nil {
		lastFailureAt := runtime.LastFailureAt.UTC()
		cloned.LastFailureAt = &lastFailureAt
	}
	return cloned
}

func Enabled(policy *Policy) bool {
	return policy != nil && policy.Type != "" && policy.Type != TypeNone
}

func NormalizePolicy(policy *Policy) (*Policy, error) {
	if policy == nil {
		return nil, nil
	}

	normalized := ClonePolicy(policy)
	if normalized.Type == "" {
		inferred, err := inferType(normalized)
		if err != nil {
			return nil, err
		}
		normalized.Type = inferred
	}
	if normalized.Type == "" {
		normalized.Type = TypeNone
	}
	if normalized.Type == TypeNone {
		normalized.HTTP = nil
		normalized.TCP = nil
		normalized.Exec = nil
		return normalized, nil
	}

	if normalized.Interval == "" {
		normalized.Interval = defaultInterval.String()
	}
	if normalized.Timeout == "" {
		normalized.Timeout = defaultTimeout.String()
	}
	if normalized.StartPeriod == "" {
		normalized.StartPeriod = defaultStartPeriod.String()
	}
	if normalized.FailureThreshold == 0 {
		normalized.FailureThreshold = defaultFailureThreshold
	}
	if normalized.SuccessThreshold == 0 {
		normalized.SuccessThreshold = defaultSuccessThreshold
	}

	interval, err := parsePositiveDuration("health_check.interval", normalized.Interval)
	if err != nil {
		return nil, err
	}
	timeout, err := parsePositiveDuration("health_check.timeout", normalized.Timeout)
	if err != nil {
		return nil, err
	}
	if timeout > interval {
		return nil, fmt.Errorf("health_check.timeout must be less than or equal to health_check.interval")
	}
	if _, err := parseNonNegativeDuration("health_check.start_period", normalized.StartPeriod); err != nil {
		return nil, err
	}
	if normalized.FailureThreshold < 1 {
		return nil, fmt.Errorf("health_check.failure_threshold must be at least 1")
	}
	if normalized.SuccessThreshold < 1 {
		return nil, fmt.Errorf("health_check.success_threshold must be at least 1")
	}

	switch normalized.Type {
	case TypeHTTP:
		if normalized.HTTP == nil {
			return nil, fmt.Errorf("health_check.http is required when type is http")
		}
		if normalized.TCP != nil || normalized.Exec != nil {
			return nil, fmt.Errorf("health_check type http cannot include tcp or exec checks")
		}
		if normalized.HTTP.Port == 0 {
			return nil, fmt.Errorf("health_check.http.port must be between 1 and 65535")
		}
		if normalized.HTTP.Path == "" {
			normalized.HTTP.Path = "/"
		}
		if normalized.HTTP.Path[0] != '/' {
			return nil, fmt.Errorf("health_check.http.path must start with /")
		}
		if normalized.HTTP.Scheme == "" {
			normalized.HTTP.Scheme = "http"
		}
		if normalized.HTTP.Scheme != "http" && normalized.HTTP.Scheme != "https" {
			return nil, fmt.Errorf("health_check.http.scheme must be http or https")
		}
		if normalized.HTTP.ExpectedStatus == 0 {
			normalized.HTTP.ExpectedStatus = 200
		}
		if normalized.HTTP.ExpectedStatus < 100 || normalized.HTTP.ExpectedStatus > 599 {
			return nil, fmt.Errorf("health_check.http.expected_status must be between 100 and 599")
		}
	case TypeTCP:
		if normalized.TCP == nil {
			return nil, fmt.Errorf("health_check.tcp is required when type is tcp")
		}
		if normalized.HTTP != nil || normalized.Exec != nil {
			return nil, fmt.Errorf("health_check type tcp cannot include http or exec checks")
		}
		if normalized.TCP.Port == 0 {
			return nil, fmt.Errorf("health_check.tcp.port must be between 1 and 65535")
		}
	case TypeExec:
		if normalized.Exec == nil {
			return nil, fmt.Errorf("health_check.exec is required when type is exec")
		}
		if normalized.HTTP != nil || normalized.TCP != nil {
			return nil, fmt.Errorf("health_check type exec cannot include http or tcp checks")
		}
		if len(normalized.Exec.Command) == 0 {
			return nil, fmt.Errorf("health_check.exec.command must not be empty")
		}
	default:
		return nil, fmt.Errorf("health_check.type must be one of none, http, tcp, exec")
	}

	return normalized, nil
}

func DurationConfig(policy *Policy) (interval, timeout, startPeriod time.Duration, err error) {
	if policy == nil {
		return defaultInterval, defaultTimeout, defaultStartPeriod, nil
	}
	interval, err = parseDurationOrDefault(policy.Interval, defaultInterval)
	if err != nil {
		return 0, 0, 0, err
	}
	timeout, err = parseDurationOrDefault(policy.Timeout, defaultTimeout)
	if err != nil {
		return 0, 0, 0, err
	}
	startPeriod, err = parseDurationOrDefault(policy.StartPeriod, defaultStartPeriod)
	if err != nil {
		return 0, 0, 0, err
	}
	return interval, timeout, startPeriod, nil
}

func inferType(policy *Policy) (Type, error) {
	var inferred Type
	count := 0
	if policy.HTTP != nil {
		inferred = TypeHTTP
		count++
	}
	if policy.TCP != nil {
		inferred = TypeTCP
		count++
	}
	if policy.Exec != nil {
		inferred = TypeExec
		count++
	}
	if count == 1 {
		return inferred, nil
	}
	if count > 1 {
		return "", fmt.Errorf("health_check must include exactly one of http, tcp, or exec when type is omitted")
	}
	return TypeNone, nil
}

func parseDurationOrDefault(value string, fallback time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	return time.ParseDuration(value)
}

func parsePositiveDuration(name, value string) (time.Duration, error) {
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s is invalid: %w", name, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return parsed, nil
}

func parseNonNegativeDuration(name, value string) (time.Duration, error) {
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s is invalid: %w", name, err)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%s must not be negative", name)
	}
	return parsed, nil
}
