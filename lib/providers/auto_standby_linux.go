//go:build linux

package providers

import (
	"context"
	"log/slog"
	"time"

	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/instances"
)

type autoStandbyInstanceStore struct {
	manager instances.Manager
}

func (s autoStandbyInstanceStore) ListInstances(ctx context.Context) ([]autostandby.Instance, error) {
	insts, err := s.manager.ListInstances(ctx, nil)
	if err != nil {
		return nil, err
	}

	out := make([]autostandby.Instance, 0, len(insts))
	for _, inst := range insts {
		out = append(out, autostandby.Instance{
			ID:             inst.Id,
			Name:           inst.Name,
			State:          string(inst.State),
			NetworkEnabled: inst.NetworkEnabled,
			IP:             inst.IP,
			HasVGPU:        inst.GPUProfile != "" || inst.GPUMdevUUID != "",
			AutoStandby:    inst.AutoStandby,
		})
	}
	return out, nil
}

func (s autoStandbyInstanceStore) StandbyInstance(ctx context.Context, id string) error {
	_, err := s.manager.StandbyInstance(ctx, id, instances.StandbyInstanceRequest{})
	return err
}

// ProvideAutoStandbyController provides the Linux auto-standby controller.
func ProvideAutoStandbyController(instanceManager instances.Manager, log *slog.Logger) *autostandby.Controller {
	if instanceManager == nil || log == nil {
		return nil
	}

	return autostandby.NewController(
		autoStandbyInstanceStore{manager: instanceManager},
		autostandby.NewConntrackSource(),
		log.With("controller", "auto_standby"),
		5*time.Second,
	)
}
