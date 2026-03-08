package instances

import (
	"context"
	"fmt"

	"github.com/kernel/hypeman/lib/egressproxy"
	"github.com/kernel/hypeman/lib/network"
)

func cloneEgressProxyConfig(cfg *EgressProxyConfig) *EgressProxyConfig {
	if cfg == nil {
		return nil
	}
	out := &EgressProxyConfig{Enabled: cfg.Enabled}
	if cfg.MockToRealEnvVar != nil {
		out.MockToRealEnvVar = make(map[string]string, len(cfg.MockToRealEnvVar))
		for mock, envVar := range cfg.MockToRealEnvVar {
			out.MockToRealEnvVar[mock] = envVar
		}
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
		InstanceID:       stored.Id,
		SourceIP:         netConfig.IP,
		TAPDevice:        netConfig.TAPDevice,
		MockToRealEnvVar: stored.EgressProxy.MockToRealEnvVar,
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
