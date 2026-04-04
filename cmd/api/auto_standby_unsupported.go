//go:build !linux

package main

import (
	"context"
	"log/slog"

	"github.com/kernel/hypeman/lib/instances"
	"golang.org/x/sync/errgroup"
)

func startAutoStandbyController(*errgroup.Group, context.Context, *slog.Logger, instances.Manager) bool {
	return false
}
