//go:build !linux

package providers

import (
	"log/slog"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/instances"
)

// ProvideAutoStandbyController is unavailable on non-Linux platforms.
func ProvideAutoStandbyController(instances.Manager, *config.Config, *slog.Logger) *autostandby.Controller {
	return nil
}
