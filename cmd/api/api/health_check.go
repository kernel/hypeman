package api

import (
	"fmt"

	"github.com/kernel/hypeman/lib/healthcheck"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/samber/lo"
)

func toDomainHealthCheck(policy *oapi.HealthCheck) (*healthcheck.Policy, error) {
	if policy == nil {
		return nil, nil
	}

	out := &healthcheck.Policy{}
	if policy.Type != nil {
		out.Type = healthcheck.Type(*policy.Type)
	}
	if policy.Interval != nil {
		out.Interval = *policy.Interval
	}
	if policy.Timeout != nil {
		out.Timeout = *policy.Timeout
	}
	if policy.StartPeriod != nil {
		out.StartPeriod = *policy.StartPeriod
	}
	if policy.FailureThreshold != nil {
		out.FailureThreshold = *policy.FailureThreshold
	}
	if policy.SuccessThreshold != nil {
		out.SuccessThreshold = *policy.SuccessThreshold
	}
	if policy.Http != nil {
		if policy.Http.Port < 1 || policy.Http.Port > 65535 {
			return nil, fmt.Errorf("health_check.http.port must be between 1 and 65535")
		}
		out.HTTP = &healthcheck.HTTPCheck{
			Port: uint16(policy.Http.Port),
		}
		if policy.Http.Path != nil {
			out.HTTP.Path = *policy.Http.Path
		}
		if policy.Http.Scheme != nil {
			out.HTTP.Scheme = string(*policy.Http.Scheme)
		}
		if policy.Http.ExpectedStatus != nil {
			out.HTTP.ExpectedStatus = *policy.Http.ExpectedStatus
		}
	}
	if policy.Tcp != nil {
		if policy.Tcp.Port < 1 || policy.Tcp.Port > 65535 {
			return nil, fmt.Errorf("health_check.tcp.port must be between 1 and 65535")
		}
		out.TCP = &healthcheck.TCPCheck{Port: uint16(policy.Tcp.Port)}
	}
	if policy.Exec != nil {
		out.Exec = &healthcheck.ExecCheck{
			Command: append([]string(nil), policy.Exec.Command...),
		}
		if policy.Exec.WorkingDir != nil {
			out.Exec.WorkingDir = *policy.Exec.WorkingDir
		}
	}

	return healthcheck.NormalizePolicy(out)
}

func toOAPIHealthCheck(policy *healthcheck.Policy) *oapi.HealthCheck {
	if policy == nil {
		return nil
	}

	typ := oapi.HealthCheckType(policy.Type)
	out := &oapi.HealthCheck{
		Type: &typ,
	}
	if policy.Interval != "" {
		out.Interval = lo.ToPtr(policy.Interval)
	}
	if policy.Timeout != "" {
		out.Timeout = lo.ToPtr(policy.Timeout)
	}
	if policy.StartPeriod != "" {
		out.StartPeriod = lo.ToPtr(policy.StartPeriod)
	}
	if policy.FailureThreshold != 0 {
		out.FailureThreshold = lo.ToPtr(policy.FailureThreshold)
	}
	if policy.SuccessThreshold != 0 {
		out.SuccessThreshold = lo.ToPtr(policy.SuccessThreshold)
	}
	if policy.HTTP != nil {
		out.Http = &oapi.HealthCheckHTTP{
			Port: int(policy.HTTP.Port),
		}
		if policy.HTTP.Path != "" {
			out.Http.Path = lo.ToPtr(policy.HTTP.Path)
		}
		if policy.HTTP.Scheme != "" {
			scheme := oapi.HealthCheckHTTPScheme(policy.HTTP.Scheme)
			out.Http.Scheme = &scheme
		}
		if policy.HTTP.ExpectedStatus != 0 {
			out.Http.ExpectedStatus = lo.ToPtr(policy.HTTP.ExpectedStatus)
		}
	}
	if policy.TCP != nil {
		out.Tcp = &oapi.HealthCheckTCP{
			Port: int(policy.TCP.Port),
		}
	}
	if policy.Exec != nil {
		out.Exec = &oapi.HealthCheckExec{
			Command: append([]string(nil), policy.Exec.Command...),
		}
		if policy.Exec.WorkingDir != "" {
			out.Exec.WorkingDir = lo.ToPtr(policy.Exec.WorkingDir)
		}
	}
	return out
}

func toOAPIHealthStatus(snapshot healthcheck.StatusSnapshot) *oapi.InstanceHealthStatus {
	out := &oapi.InstanceHealthStatus{
		Status:               oapi.InstanceHealthStatusStatus(snapshot.Status),
		ConsecutiveSuccesses: snapshot.ConsecutiveSuccesses,
		ConsecutiveFailures:  snapshot.ConsecutiveFailures,
		LastCheckedAt:        snapshot.LastCheckedAt,
		LastSuccessAt:        snapshot.LastSuccessAt,
		LastFailureAt:        snapshot.LastFailureAt,
	}
	if snapshot.LastError != "" {
		out.LastError = lo.ToPtr(snapshot.LastError)
	}
	return out
}
