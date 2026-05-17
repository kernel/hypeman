package api

import (
	"github.com/kernel/hypeman/lib/oapi"
	restartpolicy "github.com/kernel/hypeman/lib/restart-policy"
	"github.com/samber/lo"
)

func toDomainRestartPolicy(policy *oapi.RestartPolicy) (*restartpolicy.Policy, error) {
	if policy == nil {
		return nil, nil
	}

	out := &restartpolicy.Policy{}
	if policy.Policy != nil {
		out.Policy = restartpolicy.PolicyMode(*policy.Policy)
	}
	if policy.Backoff != nil {
		out.Backoff = *policy.Backoff
	}
	if policy.MaxAttempts != nil {
		out.MaxAttempts = *policy.MaxAttempts
	}
	if policy.StableAfter != nil {
		out.StableAfter = *policy.StableAfter
	}
	normalized, err := restartpolicy.NormalizePolicy(out)
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

func toOAPIRestartPolicy(policy *restartpolicy.Policy) *oapi.RestartPolicy {
	if policy == nil {
		return nil
	}

	mode := oapi.RestartPolicyPolicy(policy.Policy)
	out := &oapi.RestartPolicy{
		Policy: &mode,
	}
	if policy.Backoff != "" {
		out.Backoff = lo.ToPtr(policy.Backoff)
	}
	if policy.MaxAttempts > 0 {
		out.MaxAttempts = lo.ToPtr(policy.MaxAttempts)
	}
	if policy.StableAfter != "" {
		out.StableAfter = lo.ToPtr(policy.StableAfter)
	}
	return out
}

func toOAPIRestartStatus(status restartpolicy.Status) *oapi.RestartStatus {
	if status.IsZero() {
		return nil
	}

	out := &oapi.RestartStatus{
		Attempts: lo.ToPtr(status.Attempts),
	}
	if status.BlockedReason != "" {
		reason := oapi.RestartStatusBlockedReason(status.BlockedReason)
		out.BlockedReason = &reason
	}
	if status.LastAttemptAt != nil {
		lastAttemptAt := status.LastAttemptAt.UTC()
		out.LastAttemptAt = &lastAttemptAt
	}
	if status.NextAttemptAt != nil {
		nextAttemptAt := status.NextAttemptAt.UTC()
		out.NextAttemptAt = &nextAttemptAt
	}
	if status.LastReason != "" {
		reason := oapi.RestartStatusLastReason(status.LastReason)
		out.LastReason = &reason
	}
	return out
}
