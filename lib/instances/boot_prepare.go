package instances

import (
	"context"
	"fmt"

	"github.com/kernel/hypeman/lib/egressproxy"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/network"
	"go.opentelemetry.io/otel/attribute"
)

// prepareBootNetwork owns network allocation and its rollback for create/start.
// VZ machine networking preserves the template MAC and never allocates a Linux TAP.
func (m *manager) prepareBootNetwork(ctx context.Context, stored *StoredMetadata, req network.AllocateRequest) (*network.NetworkConfig, func(), error) {
	if !stored.NetworkEnabled {
		return nil, nil, nil
	}
	if stored.MacOS != nil {
		stored.MAC = stored.MacOS.MAC
		stored.IP = "" // vmnet addressing is observed after boot, never a static assignment.
		return &network.NetworkConfig{MAC: stored.MAC}, nil, nil
	}
	networkCtx, end := m.startLifecycleStep(ctx, "allocate_network",
		attribute.String("instance_id", stored.Id), attribute.String("hypervisor", string(stored.HypervisorType)), attribute.String("operation", "allocate_network"), attribute.Bool("network_enabled", true))
	config, err := m.networkManager.CreateAllocation(networkCtx, req)
	end(err)
	if err != nil {
		return nil, nil, fmt.Errorf("allocate network: %w", err)
	}
	stored.IP, stored.MAC = config.IP, config.MAC
	release := func() {
		allocation, err := m.networkManager.GetAllocation(ctx, stored.Id)
		if err == nil && allocation != nil {
			m.networkManager.ReleaseAllocation(ctx, allocation)
		} else {
			m.networkManager.ReleaseByInstanceID(ctx, stored.Id)
		}
	}
	return config, release, nil
}

// prepareBootConfig owns Linux proxy/config artifacts; imported machines have
// neither. The caller adds the returned proxy rollback to its cleanup stack.
func (m *manager) prepareBootConfig(ctx context.Context, stored *StoredMetadata, image *images.Image, config *network.NetworkConfig) (func(), error) {
	if stored.MacOS != nil {
		return nil, nil
	}
	var proxy *egressproxy.GuestConfig
	var err error
	proxy, err = m.maybeRegisterEgressProxy(ctx, stored, config)
	if err != nil {
		return nil, fmt.Errorf("configure egress proxy: %w", err)
	}
	var release func()
	if proxy != nil {
		release = func() { m.unregisterEgressProxyInstance(ctx, stored.Id) }
	}
	diskCtx, end := m.startLifecycleStep(ctx, "create_config_disk", attribute.String("instance_id", stored.Id), attribute.String("hypervisor", string(stored.HypervisorType)), attribute.String("operation", "create_config_disk"))
	err = m.createConfigDisk(diskCtx, &Instance{StoredMetadata: *stored}, image, config, proxy)
	end(err)
	if err != nil {
		if release != nil {
			release()
		}
		return nil, fmt.Errorf("create config disk: %w", err)
	}
	return release, nil
}
