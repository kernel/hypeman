package instances

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/kernel/hypeman/lib/egressproxy"
	"github.com/kernel/hypeman/lib/logger"
)

type updateInstanceRulesService interface {
	UpdateInstanceRules(ctx context.Context, instanceID string, rules []egressproxy.HeaderInjectRuleConfig) error
}

// updateInstance updates mutable instance properties.
// Env updates recompute egress proxy header inject rules with the new secret
// values. Auto-standby updates only change persisted metadata.
func (m *manager) updateInstance(ctx context.Context, id string, req UpdateInstanceRequest) (*Instance, error) {
	log := logger.FromContext(ctx)

	// 1. Load and validate current state
	meta, err := m.loadMetadata(id)
	if err != nil {
		log.ErrorContext(ctx, "failed to load instance metadata", "instance_id", id, "error", err)
		return nil, err
	}

	inst, err := m.getInstance(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get instance: %w", err)
	}
	normalizedAutoStandby, err := normalizeAutoStandbyPolicy(req.AutoStandby)
	if err != nil {
		return nil, err
	}
	req.AutoStandby = normalizedAutoStandby
	normalizedHealthCheck, err := normalizeHealthCheckPolicy(req.HealthCheck)
	if err != nil {
		return nil, err
	}
	req.HealthCheck = normalizedHealthCheck
	if req.RestartPolicySet {
		normalizedRestartPolicy, err := normalizeRestartPolicy(req.RestartPolicy)
		if err != nil {
			return nil, err
		}
		req.RestartPolicy = normalizedRestartPolicy
	}
	if req.NetworkEgressSet && req.NetworkEgress != nil && req.NetworkEgress.Enabled {
		mode, err := normalizeEgressEnforcementMode(req.NetworkEgress.EnforcementMode)
		if err != nil {
			return nil, err
		}
		req.NetworkEgress.EnforcementMode = mode
		proxyMode, err := normalizeEgressProxyMode(req.NetworkEgress.Proxy)
		if err != nil {
			return nil, err
		}
		req.NetworkEgress.Proxy = proxyMode
	}

	if err := validateUpdateInstanceRequest(meta, req); err != nil {
		return nil, err
	}
	if len(req.Env) > 0 && inst.State != StateRunning && inst.State != StateInitializing {
		return nil, fmt.Errorf("%w: instance must be running or initializing to update env (current state: %s)", ErrInvalidState, inst.State)
	}
	nextMeta := deepCopyMetadata(meta)
	if req.AutoStandby != nil {
		nextMeta.AutoStandby = cloneAutoStandbyPolicy(req.AutoStandby)
	}
	if req.HealthCheck != nil {
		nextMeta.HealthCheck = cloneHealthCheckPolicy(req.HealthCheck)
		nextMeta.HealthCheckRuntime = nil
	}
	if req.RestartPolicySet {
		nextMeta.RestartPolicy = cloneRestartPolicy(req.RestartPolicy)
		nextMeta.RestartStatus = restartStatusAfterPolicyUpdate(nextMeta.RestartStatus)
	}
	if req.NetworkEgressSet {
		nextMeta.NetworkEgress = cloneNetworkEgressPolicy(req.NetworkEgress)
	}
	if len(req.Env) == 0 {
		revertEgress, err := m.applyEgressEnforcementUpdate(ctx, id, inst.State, meta.NetworkEgress, nextMeta.NetworkEgress, req.NetworkEgressSet)
		if err != nil {
			return nil, err
		}
		if err := m.saveMetadata(nextMeta); err != nil {
			revertEgress()
			return nil, fmt.Errorf("save metadata: %w", err)
		}

		log.InfoContext(ctx, "instance updated", "instance_id", id)

		updated, err := m.getInstance(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("get updated instance: %w", err)
		}
		return updated, nil
	}

	prevEnv := cloneEnvMap(nextMeta.Env)
	nextEnv := cloneEnvMap(nextMeta.Env)
	if nextEnv == nil {
		nextEnv = make(map[string]string)
	}
	for k, v := range req.Env {
		nextEnv[k] = v
	}

	if err := validateCredentialEnvBindings(nextMeta.Credentials, nextEnv); err != nil {
		return nil, err
	}

	svc := m.getEgressProxyIfExists()
	if svc == nil {
		log.ErrorContext(ctx, "egress proxy service unavailable for credential update", "instance_id", id)
		return nil, fmt.Errorf("egress proxy service unavailable")
	}

	if err := applyUpdatedInstanceEnv(ctx, log, id, nextMeta, prevEnv, nextEnv, m.saveMetadata, svc); err != nil {
		return nil, err
	}

	log.InfoContext(ctx, "instance updated", "instance_id", id)

	updated, err := m.getInstance(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get updated instance: %w", err)
	}
	return updated, nil
}

func validateUpdateInstanceRequest(meta *metadata, req UpdateInstanceRequest) error {
	if len(req.Env) == 0 && req.AutoStandby == nil && req.HealthCheck == nil && !req.RestartPolicySet && !req.NetworkEgressSet {
		return fmt.Errorf("%w: request must include env, auto_standby, health_check, restart_policy, and/or egress", ErrInvalidRequest)
	}
	if req.NetworkEgressSet {
		if meta == nil {
			return fmt.Errorf("%w: instance metadata is required", ErrInvalidRequest)
		}
		if meta.NetworkEgress != nil && meta.NetworkEgress.Enabled && egressProxyMode(meta.NetworkEgress) == EgressProxyModeMITM {
			return fmt.Errorf("%w: egress policy of instances using proxy=mitm cannot be updated; recreate the instance", ErrInvalidRequest)
		}
		if req.NetworkEgress != nil && req.NetworkEgress.Enabled {
			if !meta.NetworkEnabled {
				return fmt.Errorf("%w: egress requires network.enabled=true", ErrInvalidRequest)
			}
			if req.NetworkEgress.Proxy != EgressProxyModeNone {
				return fmt.Errorf("%w: only enforcement-only egress (proxy=none) can be set via update", ErrInvalidRequest)
			}
		}
	}
	if req.HealthCheck != nil {
		if meta == nil {
			return fmt.Errorf("%w: instance metadata is required", ErrInvalidRequest)
		}
		if err := validateHealthCheckCompatibility(req.HealthCheck, meta.NetworkEnabled, meta.SkipGuestAgent); err != nil {
			return err
		}
	}
	if len(req.Env) == 0 {
		return nil
	}
	if meta == nil || len(meta.Credentials) == 0 || meta.NetworkEgress == nil || !meta.NetworkEgress.Enabled {
		return fmt.Errorf("%w: instance has no credential-backed env vars to update", ErrInvalidRequest)
	}

	allowedNames := credentialSourceEnvNames(meta.Credentials)
	if len(allowedNames) == 0 {
		return fmt.Errorf("%w: instance has no credential-backed env vars to update", ErrInvalidRequest)
	}
	allowedSet := make(map[string]struct{}, len(allowedNames))
	for _, name := range allowedNames {
		allowedSet[name] = struct{}{}
	}

	invalidKeys := make([]string, 0)
	for key := range req.Env {
		if _, ok := allowedSet[key]; ok {
			continue
		}
		invalidKeys = append(invalidKeys, key)
	}
	if len(invalidKeys) > 0 {
		sort.Strings(invalidKeys)
		return fmt.Errorf("%w: env keys %v are not credential source env vars; allowed keys: %v", ErrInvalidRequest, invalidKeys, allowedNames)
	}

	return nil
}

// applyEgressEnforcementUpdate reconciles host-side egress enforcement with the
// next policy for a live instance, returning a revert func that restores the
// previous enforcement state if the metadata save fails. Policies that require
// the MITM proxy are rejected by validation before this point, so only iptables
// state changes here — nothing guest-visible.
func (m *manager) applyEgressEnforcementUpdate(ctx context.Context, id string, state State, prev, next *NetworkEgressPolicy, set bool) (func(), error) {
	noop := func() {}
	if !set {
		return noop, nil
	}
	if state != StateRunning && state != StateInitializing {
		// Stopped/standby instances pick up the persisted policy on next
		// start/restore via maybeRegisterEgressProxy.
		return noop, nil
	}

	alloc, err := m.networkManager.GetAllocation(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get network allocation for egress update: %w", err)
	}
	if alloc == nil {
		return nil, fmt.Errorf("%w: instance has no active network allocation", ErrInvalidState)
	}

	apply := func(policy *NetworkEgressPolicy) error {
		if policy == nil || !policy.Enabled {
			return egressproxy.RemoveEnforcement(id)
		}
		blockAllTCP, blockUDP := egressBlockFlags(policy.EnforcementMode)
		return egressproxy.ApplyEnforcement(id, alloc.TAPDevice, alloc.Gateway, blockAllTCP, blockUDP)
	}

	if err := apply(next); err != nil {
		return nil, fmt.Errorf("apply egress enforcement: %w", err)
	}
	revert := func() {
		if err := apply(prev); err != nil {
			logger.FromContext(ctx).WarnContext(ctx, "failed to revert egress enforcement after metadata save failure", "instance_id", id, "error", err)
		}
	}
	return revert, nil
}

func cloneEnvMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func applyUpdatedInstanceEnv(ctx context.Context, log *slog.Logger, instanceID string, meta *metadata, prevEnv map[string]string, nextEnv map[string]string, save func(*metadata) error, svc updateInstanceRulesService) error {
	if log == nil {
		log = logger.FromContext(ctx)
	}

	if svc == nil {
		return fmt.Errorf("egress proxy service unavailable")
	}

	oldRules := buildEgressProxyInjectRules(meta.NetworkEgress, meta.Credentials, prevEnv)
	newRules := buildEgressProxyInjectRules(meta.NetworkEgress, meta.Credentials, nextEnv)

	if err := svc.UpdateInstanceRules(ctx, instanceID, newRules); err != nil {
		return fmt.Errorf("update egress proxy rules: %w", err)
	}
	log.DebugContext(ctx, "updated egress proxy header inject rules", "instance_id", instanceID)

	meta.Env = nextEnv
	if err := save(meta); err != nil {
		if rollbackErr := svc.UpdateInstanceRules(ctx, instanceID, oldRules); rollbackErr != nil {
			meta.Env = prevEnv
			return fmt.Errorf("save metadata: %w (failed to roll back egress proxy rules: %v)", err, rollbackErr)
		}
		meta.Env = prevEnv
		log.WarnContext(ctx, "rolled back egress proxy header inject rules after metadata save failure", "instance_id", instanceID, "error", err)
		return fmt.Errorf("save metadata: %w", err)
	}

	return nil
}
