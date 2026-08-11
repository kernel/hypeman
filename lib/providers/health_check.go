package providers

import (
	"log/slog"

	"github.com/kernel/hypeman/lib/instances"
)

func ProvideHealthCheckController(instanceManager instances.Manager, log *slog.Logger) *instances.HealthCheckController {
	finish := trackInitialization(log, "health check controller")
	defer func() { finish(nil) }()

	return instances.NewHealthCheckController(instanceManager, log)
}
