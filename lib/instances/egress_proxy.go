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

func buildEgressProxyReplacements(cfg *EgressProxyConfig, env map[string]string) map[string]string {
	if cfg == nil || !cfg.Enabled || len(cfg.MockEnvVars) == 0 {
		return nil
	}
	out := make(map[string]string, len(cfg.MockEnvVars))
	for _, envVar := range cfg.MockEnvVars {
		real := env[envVar]
		if real == "" {
			continue
		}
		out[mockValueForEnvVar(envVar)] = real
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

	svc, err := egressproxy.NewService(m.paths.DataDir(), egressproxy.DefaultListenPort)
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
		InstanceID:            stored.Id,
		SourceIP:              netConfig.IP,
		TAPDevice:             netConfig.TAPDevice,
		BlockAllTCPEgress:     stored.EgressProxy.EnforcementMode != EgressProxyEnforcementModeHTTPHTTPSOnly,
		MockToRealSecretValue: buildEgressProxyReplacements(stored.EgressProxy, stored.Env),
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
