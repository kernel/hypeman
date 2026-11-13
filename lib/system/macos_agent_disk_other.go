//go:build !darwin

package system

import (
	"context"
	"errors"

	"github.com/kernel/hypeman/lib/paths"
)

// MacOSAgentDisk is only available on macOS hosts.
func MacOSAgentDisk(context.Context, *paths.Paths) (string, error) {
	return "", errors.New("the macOS guest agent disk requires a macOS host")
}
