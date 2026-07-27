//go:build linux

package providers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/instances"
	"go.opentelemetry.io/otel"
)

type autoStandbyRuntimeManager interface {
	GetAutoStandbyRuntime(ctx context.Context, id string) (*autostandby.Runtime, error)
	SetAutoStandbyRuntime(ctx context.Context, id string, runtime *autostandby.Runtime) error
	SubscribeLifecycleEvents(consumer instances.LifecycleEventConsumer) (<-chan instances.LifecycleEvent, func())
}

type autoStandbyInstanceStore struct {
	manager        instances.Manager
	runtimeManager autoStandbyRuntimeManager
}

func (s autoStandbyInstanceStore) ListInstances(ctx context.Context) ([]autostandby.Instance, error) {
	insts, err := s.manager.ListInstances(ctx, nil)
	if err != nil {
		return nil, err
	}

	out := make([]autostandby.Instance, 0, len(insts))
	for _, inst := range insts {
		runtime, err := s.runtimeManager.GetAutoStandbyRuntime(ctx, inst.Id)
		if err != nil {
			return nil, err
		}

		out = append(out, autostandby.Instance{
			ID:             inst.Id,
			Name:           inst.Name,
			State:          string(inst.State),
			NetworkEnabled: inst.NetworkEnabled,
			IP:             inst.IP,
			HasVGPU:        inst.GPUProfile != "" || inst.GPUMdevUUID != "",
			AutoStandby:    inst.AutoStandby,
			Runtime:        runtime,
		})
	}
	return out, nil
}

func (s autoStandbyInstanceStore) StandbyInstance(ctx context.Context, id string) error {
	_, err := s.manager.StandbyInstance(ctx, id, instances.StandbyInstanceRequest{})
	if errors.Is(err, instances.ErrNotFound) {
		return fmt.Errorf("%w: %v", autostandby.ErrInstanceNotFound, err)
	}
	return err
}

func (s autoStandbyInstanceStore) RestoreInstance(ctx context.Context, id string) error {
	_, err := s.manager.RestoreInstance(ctx, id)
	if errors.Is(err, instances.ErrNotFound) {
		return fmt.Errorf("%w: %v", autostandby.ErrInstanceNotFound, err)
	}
	return err
}

func (s autoStandbyInstanceStore) SetRuntime(ctx context.Context, id string, runtime *autostandby.Runtime) error {
	return s.runtimeManager.SetAutoStandbyRuntime(ctx, id, runtime)
}

func (s autoStandbyInstanceStore) SubscribeInstanceEvents() (<-chan autostandby.InstanceEvent, func(), error) {
	src, unsub := s.runtimeManager.SubscribeLifecycleEvents(instances.LifecycleEventConsumerAutoStandby)
	dst := make(chan autostandby.InstanceEvent, 32)
	go func() {
		defer close(dst)
		for event := range src {
			inst := toAutoStandbyInstance(event.Instance)
			if inst != nil {
				runtime, err := s.runtimeManager.GetAutoStandbyRuntime(context.Background(), inst.ID)
				if err == nil {
					inst.Runtime = runtime
				}
			}
			dst <- autostandby.InstanceEvent{
				Action:     autostandby.InstanceEventAction(event.Action),
				InstanceID: event.InstanceID,
				Instance:   inst,
			}
		}
	}()
	return dst, unsub, nil
}

func toAutoStandbyInstance(inst *instances.Instance) *autostandby.Instance {
	if inst == nil {
		return nil
	}
	return &autostandby.Instance{
		ID:             inst.Id,
		Name:           inst.Name,
		State:          string(inst.State),
		NetworkEnabled: inst.NetworkEnabled,
		IP:             inst.IP,
		HasVGPU:        inst.GPUProfile != "" || inst.GPUMdevUUID != "",
		AutoStandby:    inst.AutoStandby,
	}
}

// ProvideAutoStandbyController provides the Linux auto-standby controller.
func ProvideAutoStandbyController(instanceManager instances.Manager, cfg *config.Config, log *slog.Logger) *autostandby.Controller {
	if instanceManager == nil || log == nil {
		return nil
	}

	runtimeManager, ok := instanceManager.(autoStandbyRuntimeManager)
	if !ok {
		return nil
	}

	return autostandby.NewController(
		autoStandbyInstanceStore{manager: instanceManager, runtimeManager: runtimeManager},
		autostandby.NewConntrackSource(),
		autostandby.ControllerOptions{
			Log:                   log.With("controller", "auto_standby"),
			Meter:                 otel.GetMeterProvider().Meter("hypeman/autostandby"),
			Tracer:                otel.GetTracerProvider().Tracer("hypeman/autostandby"),
			MaxConcurrentStandbys: cfg.AutoStandby.MaxConcurrent,
		},
	)
}
