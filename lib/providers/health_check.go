package providers

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/healthcheck"
	"github.com/kernel/hypeman/lib/instances"
)

type healthCheckRuntimeManager interface {
	GetHealthCheckRuntime(ctx context.Context, id string) (*healthcheck.Runtime, error)
	SetHealthCheckRuntime(ctx context.Context, id string, runtime *healthcheck.Runtime) error
	SubscribeLifecycleEvents(consumer instances.LifecycleEventConsumer) (<-chan instances.LifecycleEvent, func())
}

type healthCheckInstanceStore struct {
	manager        instances.Manager
	runtimeManager healthCheckRuntimeManager
}

func (s healthCheckInstanceStore) ListInstances(ctx context.Context) ([]healthcheck.Instance, error) {
	insts, err := s.manager.ListInstances(ctx, nil)
	if err != nil {
		return nil, err
	}

	out := make([]healthcheck.Instance, 0, len(insts))
	for _, inst := range insts {
		runtime, err := s.runtimeManager.GetHealthCheckRuntime(ctx, inst.Id)
		if err != nil {
			return nil, err
		}
		out = append(out, toHealthCheckInstance(&inst, runtime))
	}
	return out, nil
}

func (s healthCheckInstanceStore) SetRuntime(ctx context.Context, id string, runtime *healthcheck.Runtime) error {
	return s.runtimeManager.SetHealthCheckRuntime(ctx, id, runtime)
}

func (s healthCheckInstanceStore) SubscribeInstanceEvents() (<-chan healthcheck.InstanceEvent, func(), error) {
	src, unsub := s.runtimeManager.SubscribeLifecycleEvents(instances.LifecycleEventConsumerHealthCheck)
	dst := make(chan healthcheck.InstanceEvent, 32)
	go func() {
		defer close(dst)
		for event := range src {
			var inst *healthcheck.Instance
			if event.Instance != nil {
				runtime, err := s.runtimeManager.GetHealthCheckRuntime(context.Background(), event.Instance.Id)
				if err == nil {
					converted := toHealthCheckInstance(event.Instance, runtime)
					inst = &converted
				}
			}
			dst <- healthcheck.InstanceEvent{
				Action:     healthcheck.InstanceEventAction(event.Action),
				InstanceID: event.InstanceID,
				Instance:   inst,
			}
		}
	}()
	return dst, unsub, nil
}

type healthCheckExecRunner struct {
	manager instances.Manager
}

func (r healthCheckExecRunner) Run(ctx context.Context, inst healthcheck.Instance, check healthcheck.ExecCheck, timeout time.Duration) error {
	dialer, err := r.manager.GetVsockDialer(ctx, inst.ID)
	if err != nil {
		return err
	}

	timeoutSeconds := int32((timeout + time.Second - 1) / time.Second)
	if timeoutSeconds < 1 {
		timeoutSeconds = 1
	}
	exit, err := guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command: check.Command,
		Cwd:     check.WorkingDir,
		Timeout: timeoutSeconds,
	})
	if err != nil {
		return err
	}
	if exit == nil {
		return fmt.Errorf("exec health check exited without status")
	}
	if exit.Code != 0 {
		return fmt.Errorf("exec health check exited with status %d", exit.Code)
	}
	return nil
}

func toHealthCheckInstance(inst *instances.Instance, runtime *healthcheck.Runtime) healthcheck.Instance {
	if inst == nil {
		return healthcheck.Instance{}
	}
	return healthcheck.Instance{
		ID:              inst.Id,
		Name:            inst.Name,
		State:           string(inst.State),
		NetworkEnabled:  inst.NetworkEnabled,
		IP:              inst.IP,
		StartedAt:       inst.StartedAt,
		GuestAgentReady: inst.GuestAgentReadyAt != nil,
		SkipGuestAgent:  inst.SkipGuestAgent,
		HealthCheck:     inst.HealthCheck,
		Runtime:         runtime,
	}
}

func ProvideHealthCheckController(instanceManager instances.Manager, log *slog.Logger) *healthcheck.Controller {
	if instanceManager == nil || log == nil {
		return nil
	}
	runtimeManager, ok := instanceManager.(healthCheckRuntimeManager)
	if !ok {
		return nil
	}

	return healthcheck.NewController(
		healthCheckInstanceStore{manager: instanceManager, runtimeManager: runtimeManager},
		healthcheck.DefaultProbeRunner{ExecRunner: healthCheckExecRunner{manager: instanceManager}},
		healthcheck.ControllerOptions{Log: log.With("controller", "health_check")},
	)
}
