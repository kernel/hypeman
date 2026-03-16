package instances

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/kernel/hypeman/lib/egressproxy"
	"github.com/kernel/hypeman/lib/network"
)

const mockSecretPrefix = "mock-"

func cloneNetworkEgressPolicy(cfg *NetworkEgressPolicy) *NetworkEgressPolicy {
	if cfg == nil {
		return nil
	}
	return &NetworkEgressPolicy{
		Enabled:         cfg.Enabled,
		EnforcementMode: cfg.EnforcementMode,
	}
}

func cloneCredentialPolicies(in map[string]CredentialPolicy) map[string]CredentialPolicy {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]CredentialPolicy, len(in))
	for name, policy := range in {
		cloned := CredentialPolicy{
			Source: CredentialSource{Env: policy.Source.Env},
		}
		if len(policy.Inject) > 0 {
			cloned.Inject = make([]CredentialInjectRule, 0, len(policy.Inject))
			for _, inject := range policy.Inject {
				next := CredentialInjectRule{
					As: CredentialInjectAs{
						Header: inject.As.Header,
						Format: inject.As.Format,
					},
				}
				if len(inject.Hosts) > 0 {
					next.Hosts = append([]string(nil), inject.Hosts...)
				}
				cloned.Inject = append(cloned.Inject, next)
			}
		}
		out[name] = cloned
	}
	return out
}

func mockValueForCredential(name string) string {
	return mockSecretPrefix + name
}

func normalizeEgressEnforcementMode(mode EgressEnforcementMode) (EgressEnforcementMode, error) {
	trimmed := strings.TrimSpace(string(mode))
	switch EgressEnforcementMode(trimmed) {
	case "", EgressEnforcementModeAll:
		return EgressEnforcementModeAll, nil
	case EgressEnforcementModeHTTPHTTPSOnly:
		return EgressEnforcementModeHTTPHTTPSOnly, nil
	default:
		return "", fmt.Errorf("%w: invalid network.egress.enforcement.mode %q", ErrInvalidRequest, trimmed)
	}
}

func normalizeCredentialPolicies(in map[string]CredentialPolicy) (map[string]CredentialPolicy, error) {
	if len(in) == 0 {
		return nil, nil
	}

	out := make(map[string]CredentialPolicy, len(in))
	for rawCredentialName, rawPolicy := range in {
		credentialName := strings.TrimSpace(rawCredentialName)
		if credentialName == "" {
			return nil, fmt.Errorf("%w: credentials key must be non-empty", ErrInvalidRequest)
		}
		if _, exists := out[credentialName]; exists {
			return nil, fmt.Errorf("%w: duplicate credentials key %q after normalization", ErrInvalidRequest, credentialName)
		}

		sourceEnv := strings.TrimSpace(rawPolicy.Source.Env)
		if sourceEnv == "" {
			return nil, fmt.Errorf("%w: credentials[%q].source.env is required", ErrInvalidRequest, credentialName)
		}
		if len(rawPolicy.Inject) == 0 {
			return nil, fmt.Errorf("%w: credentials[%q].inject must have at least one rule", ErrInvalidRequest, credentialName)
		}

		policy := CredentialPolicy{
			Source: CredentialSource{Env: sourceEnv},
			Inject: make([]CredentialInjectRule, 0, len(rawPolicy.Inject)),
		}
		for idx, rawInject := range rawPolicy.Inject {
			header := strings.TrimSpace(rawInject.As.Header)
			if header == "" {
				return nil, fmt.Errorf("%w: credentials[%q].inject[%d].as.header is required", ErrInvalidRequest, credentialName, idx)
			}
			format := strings.TrimSpace(rawInject.As.Format)
			if format == "" {
				return nil, fmt.Errorf("%w: credentials[%q].inject[%d].as.format is required", ErrInvalidRequest, credentialName, idx)
			}
			if !strings.Contains(format, "${value}") {
				return nil, fmt.Errorf("%w: credentials[%q].inject[%d].as.format must include ${value}", ErrInvalidRequest, credentialName, idx)
			}

			hosts, err := normalizeCredentialHosts(rawInject.Hosts)
			if err != nil {
				return nil, fmt.Errorf("%w: credentials[%q].inject[%d].hosts: %v", ErrInvalidRequest, credentialName, idx, err)
			}
			policy.Inject = append(policy.Inject, CredentialInjectRule{
				Hosts: hosts,
				As: CredentialInjectAs{
					Header: header,
					Format: format,
				},
			})
		}

		out[credentialName] = policy
	}

	return out, nil
}

func normalizeCredentialHosts(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, rawPattern := range in {
		normalized, err := egressproxy.NormalizeAllowedDomainPattern(rawPattern)
		if err != nil {
			return nil, fmt.Errorf("invalid host pattern %q: %w", rawPattern, err)
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out, nil
}

func validateCredentialEnvBindings(credentials map[string]CredentialPolicy, env map[string]string) error {
	for credentialName, policy := range credentials {
		real, ok := env[policy.Source.Env]
		if !ok {
			return fmt.Errorf("%w: credentials[%q].source.env %q must be present in env", ErrInvalidRequest, credentialName, policy.Source.Env)
		}
		if strings.TrimSpace(real) == "" {
			return fmt.Errorf("%w: env var %q must be non-empty when referenced by credentials[%q]", ErrInvalidRequest, policy.Source.Env, credentialName)
		}
	}
	return nil
}

func buildEgressProxyRewriteRules(egressPolicy *NetworkEgressPolicy, credentials map[string]CredentialPolicy, env map[string]string) []egressproxy.SecretRewriteRuleConfig {
	if egressPolicy == nil || !egressPolicy.Enabled || len(credentials) == 0 {
		return nil
	}
	names := make([]string, 0, len(credentials))
	for name := range credentials {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]egressproxy.SecretRewriteRuleConfig, 0, len(names))
	for _, credentialName := range names {
		policy := credentials[credentialName]
		real := strings.TrimSpace(env[policy.Source.Env])
		if real == "" {
			continue
		}

		rule := egressproxy.SecretRewriteRuleConfig{
			MockValue: mockValueForCredential(credentialName),
			RealValue: real,
		}

		domains, unrestricted := collectCredentialAllowedDomains(policy.Inject)
		if !unrestricted && len(domains) > 0 {
			rule.AllowedDomains = domains
		}

		out = append(out, rule)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func collectCredentialAllowedDomains(injectRules []CredentialInjectRule) ([]string, bool) {
	if len(injectRules) == 0 {
		return nil, true
	}
	seen := map[string]struct{}{}
	out := make([]string, 0)
	for _, inject := range injectRules {
		// Empty hosts means "all destinations".
		if len(inject.Hosts) == 0 {
			return nil, true
		}
		for _, host := range inject.Hosts {
			if _, ok := seen[host]; ok {
				continue
			}
			seen[host] = struct{}{}
			out = append(out, host)
		}
	}
	return out, false
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
	if stored == nil || stored.NetworkEgress == nil || !stored.NetworkEgress.Enabled {
		return nil, nil
	}
	if !stored.NetworkEnabled || netConfig == nil {
		return nil, fmt.Errorf("network.egress requires network.enabled=true")
	}

	svc, err := m.getOrCreateEgressProxyService()
	if err != nil {
		return nil, fmt.Errorf("create egress proxy service: %w", err)
	}

	guestCfg, err := svc.RegisterInstance(ctx, netConfig.Gateway, egressproxy.InstanceConfig{
		InstanceID:         stored.Id,
		SourceIP:           netConfig.IP,
		TAPDevice:          netConfig.TAPDevice,
		BlockAllTCPEgress:  stored.NetworkEgress.EnforcementMode != EgressEnforcementModeHTTPHTTPSOnly,
		SecretRewriteRules: buildEgressProxyRewriteRules(stored.NetworkEgress, stored.Credentials, stored.Env),
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
