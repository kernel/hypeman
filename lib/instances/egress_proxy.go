package instances

import (
	"context"
	"fmt"
	"strings"

	"github.com/kernel/hypeman/lib/egressproxy"
	"github.com/kernel/hypeman/lib/network"
)

const mockSecretPrefix = "mock-"

func cloneEgressProxyConfig(cfg *EgressProxyConfig) *EgressProxyConfig {
	if cfg == nil {
		return nil
	}
	out := &EgressProxyConfig{
		Enabled:         cfg.Enabled,
		EnforcementMode: cfg.EnforcementMode,
	}
	if cfg.MockEnvVars != nil {
		out.MockEnvVars = append([]string(nil), cfg.MockEnvVars...)
	}
	if cfg.MockEnvVarDomains != nil {
		out.MockEnvVarDomains = cloneMockEnvVarDomains(cfg.MockEnvVarDomains)
	}
	return out
}

func normalizeMockEnvVars(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}

	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, name := range in {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return nil, fmt.Errorf("%w: egress proxy mock_env_vars entries must be non-empty", ErrInvalidRequest)
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out, nil
}

func mockValueForEnvVar(name string) string {
	return mockSecretPrefix + name
}

func cloneMockEnvVarDomains(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for envVar, patterns := range in {
		out[envVar] = append([]string(nil), patterns...)
	}
	return out
}

func normalizeEgressProxyEnforcementMode(mode EgressProxyEnforcementMode) (EgressProxyEnforcementMode, error) {
	trimmed := strings.TrimSpace(string(mode))
	switch EgressProxyEnforcementMode(trimmed) {
	case "", EgressProxyEnforcementModeAll:
		return EgressProxyEnforcementModeAll, nil
	case EgressProxyEnforcementModeHTTPHTTPSOnly:
		return EgressProxyEnforcementModeHTTPHTTPSOnly, nil
	default:
		return "", fmt.Errorf("%w: invalid egress proxy enforcement_mode %q", ErrInvalidRequest, trimmed)
	}
}

func normalizeMockEnvVarDomains(mockEnvVars []string, in map[string][]string) (map[string][]string, error) {
	if len(in) == 0 {
		return nil, nil
	}

	allowedVars := make(map[string]struct{}, len(mockEnvVars))
	for _, name := range mockEnvVars {
		allowedVars[name] = struct{}{}
	}

	out := make(map[string][]string, len(in))
	for rawEnvVar, rawPatterns := range in {
		envVar := strings.TrimSpace(rawEnvVar)
		if envVar == "" {
			return nil, fmt.Errorf("%w: egress proxy mock_env_var_domains key must be non-empty", ErrInvalidRequest)
		}
		if _, ok := allowedVars[envVar]; !ok {
			return nil, fmt.Errorf("%w: egress proxy mock_env_var_domains key %q must be present in mock_env_vars", ErrInvalidRequest, envVar)
		}
		if len(rawPatterns) == 0 {
			return nil, fmt.Errorf("%w: egress proxy mock_env_var_domains[%q] must have at least one domain pattern", ErrInvalidRequest, envVar)
		}

		seen := make(map[string]struct{}, len(rawPatterns))
		patterns := make([]string, 0, len(rawPatterns))
		for _, rawPattern := range rawPatterns {
			normalized, err := egressproxy.NormalizeAllowedDomainPattern(rawPattern)
			if err != nil {
				return nil, fmt.Errorf("%w: invalid egress proxy domain pattern %q for %q: %v", ErrInvalidRequest, rawPattern, envVar, err)
			}
			if _, ok := seen[normalized]; ok {
				continue
			}
			seen[normalized] = struct{}{}
			patterns = append(patterns, normalized)
		}
		if len(patterns) == 0 {
			return nil, fmt.Errorf("%w: egress proxy mock_env_var_domains[%q] must include at least one valid domain pattern", ErrInvalidRequest, envVar)
		}
		out[envVar] = patterns
	}
	return out, nil
}

func buildEgressProxyRewriteRules(cfg *EgressProxyConfig, env map[string]string) []egressproxy.SecretRewriteRuleConfig {
	if cfg == nil || !cfg.Enabled || len(cfg.MockEnvVars) == 0 {
		return nil
	}
	out := make([]egressproxy.SecretRewriteRuleConfig, 0, len(cfg.MockEnvVars))
	for _, envVar := range cfg.MockEnvVars {
		real := env[envVar]
		if real == "" {
			continue
		}
		rule := egressproxy.SecretRewriteRuleConfig{
			MockValue: mockValueForEnvVar(envVar),
			RealValue: real,
		}
		if cfg.MockEnvVarDomains != nil && len(cfg.MockEnvVarDomains[envVar]) > 0 {
			rule.AllowedDomains = append([]string(nil), cfg.MockEnvVarDomains[envVar]...)
		}
		out = append(out, rule)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (m *manager) getOrCreateEgressProxyService() (*egressproxy.Service, error) {
	m.egressProxyMu.Lock()
	defer m.egressProxyMu.Unlock()

	if m.egressProxy != nil {
		return m.egressProxy, nil
	}

	svc, err := egressproxy.NewServiceWithOptions(m.paths.DataDir(), egressproxy.DefaultListenPort, m.egressProxyServiceOptions)
	if err != nil {
		return nil, err
	}
	m.egressProxy = svc
	return svc, nil
}

func (m *manager) maybeRegisterEgressProxy(ctx context.Context, stored *StoredMetadata, netConfig *network.NetworkConfig) (*egressproxy.GuestConfig, error) {
	if stored == nil || stored.EgressProxy == nil || !stored.EgressProxy.Enabled {
		return nil, nil
	}
	if !stored.NetworkEnabled || netConfig == nil {
		return nil, fmt.Errorf("egress proxy requires network_enabled=true")
	}

	svc, err := m.getOrCreateEgressProxyService()
	if err != nil {
		return nil, fmt.Errorf("create egress proxy service: %w", err)
	}

	guestCfg, err := svc.RegisterInstance(ctx, netConfig.Gateway, egressproxy.InstanceConfig{
		InstanceID:         stored.Id,
		SourceIP:           netConfig.IP,
		TAPDevice:          netConfig.TAPDevice,
		BlockAllTCPEgress:  stored.EgressProxy.EnforcementMode != EgressProxyEnforcementModeHTTPHTTPSOnly,
		SecretRewriteRules: buildEgressProxyRewriteRules(stored.EgressProxy, stored.Env),
	})
	if err != nil {
		return nil, fmt.Errorf("register instance with egress proxy: %w", err)
	}

	return &guestCfg, nil
}

func (m *manager) unregisterEgressProxyInstance(ctx context.Context, instanceID string) {
	_ = ctx
	m.egressProxyMu.Lock()
	svc := m.egressProxy
	m.egressProxyMu.Unlock()
	if svc == nil {
		return
	}
	svc.UnregisterInstance(context.Background(), instanceID)
}
