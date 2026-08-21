package instances

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/kernel/hypeman/lib/egressproxy"
	"github.com/kernel/hypeman/lib/logger"
)

type updateInstanceRulesService interface {
	UpdateInstanceRules(ctx context.Context, instanceID string, rules []egressproxy.HeaderInjectRuleConfig) error
}

// updateInstance updates mutable instance properties.
// Env updates recompute egress proxy header inject rules with the new secret
// values. Other updates only change persisted metadata.
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

	if err := validateUpdateInstanceRequest(meta, req); err != nil {
		return nil, err
	}
	if err := validateExpirationUpdate(meta, req, m.nowUTC()); err != nil {
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
	if len(req.Env) == 0 {
		if err := applyExpirationUpdateAtCommit(nextMeta, req, m.nowUTC()); err != nil {
			return nil, err
		}
		if err := m.saveMetadata(nextMeta); err != nil {
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

	saveAtCommit := func(meta *metadata) error {
		if err := applyExpirationUpdateAtCommit(meta, req, m.nowUTC()); err != nil {
			return err
		}
		return m.saveMetadata(meta)
	}
	if err := applyUpdatedInstanceEnv(ctx, log, id, nextMeta, prevEnv, nextEnv, saveAtCommit, svc); err != nil {
		return nil, err
	}

	log.InfoContext(ctx, "instance updated", "instance_id", id)

	updated, err := m.getInstance(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get updated instance: %w", err)
	}
	return updated, nil
}

func applyExpirationUpdateAtCommit(meta *metadata, req UpdateInstanceRequest, now time.Time) error {
	expiresAt, expirationSet, err := resolveExpirationUpdate(meta, req, now)
	if err != nil {
		return err
	}
	if expirationSet {
		meta.ExpiresAt = expiresAt
	}
	return nil
}

func validateExpirationUpdate(meta *metadata, req UpdateInstanceRequest, now time.Time) error {
	if req.TTL == nil && req.ExpiresAt == nil {
		return nil
	}
	if instanceExpired(&meta.StoredMetadata, now) {
		return fmt.Errorf("%w: expiration deadline has passed", ErrInstanceExpired)
	}
	return validateExpiresAt(req.ExpiresAt, now)
}

func resolveExpirationUpdate(meta *metadata, req UpdateInstanceRequest, now time.Time) (*time.Time, bool, error) {
	if err := validateExpirationUpdate(meta, req, now); err != nil {
		return nil, false, err
	}
	if req.TTL == nil && req.ExpiresAt == nil {
		return nil, false, nil
	}
	if req.TTL != nil {
		if *req.TTL == 0 {
			return nil, true, nil
		}
		expiresAt := now.Add(*req.TTL)
		return &expiresAt, true, nil
	}
	expiresAt := req.ExpiresAt.UTC()
	return &expiresAt, true, nil
}

func validateUpdateInstanceRequest(meta *metadata, req UpdateInstanceRequest) error {
	if len(req.Env) == 0 && req.AutoStandby == nil && req.HealthCheck == nil && !req.RestartPolicySet && req.TTL == nil && req.ExpiresAt == nil {
		return fmt.Errorf("%w: request must include env, auto_standby, health_check, restart_policy, ttl, and/or expires_at", ErrInvalidRequest)
	}
	if req.TTL != nil && req.ExpiresAt != nil {
		return fmt.Errorf("%w: ttl and expires_at are mutually exclusive", ErrInvalidRequest)
	}
	if req.TTL != nil && *req.TTL < 0 {
		return fmt.Errorf("%w: ttl cannot be negative", ErrInvalidRequest)
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
