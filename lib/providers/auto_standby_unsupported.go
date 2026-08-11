//go:build !linux

package providers

import (
	"log/slog"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/instances"
)

// ProvideAutoStandbyController is unavailable on non-Linux platforms.
func ProvideAutoStandbyController(_ instances.Manager, _ *config.Config, log *slog.Logger) *autostandby.Controller {
	if log == nil {
		return nil
	}

	finish := trackInitialization(log, "auto-standby controller")
	defer func() { finish(nil) }()

	return nil
}
