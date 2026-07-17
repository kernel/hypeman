package providers

import (
	"log/slog"

	"github.com/kernel/hypeman/lib/instances"
)

func ProvideHealthCheckController(instanceManager instances.Manager, log *slog.Logger) *instances.HealthCheckController {
	return instances.NewHealthCheckController(instanceManager, log)
}
