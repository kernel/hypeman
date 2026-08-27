package providers

import (
	"log/slog"

	"github.com/kernel/hypeman/lib/instances"
	"go.opentelemetry.io/otel"
)

func ProvideVGPUSentinelController(instanceManager instances.Manager, log *slog.Logger) (*instances.VGPUSentinelController, error) {
	meter := otel.GetMeterProvider().Meter("hypeman")
	return instances.NewVGPUSentinelController(instanceManager, meter, log)
}
