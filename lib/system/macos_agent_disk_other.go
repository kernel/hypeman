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

// BuildMacOSConfigDisk is only available on macOS hosts.
func BuildMacOSConfigDisk(context.Context, map[string]string, string) error {
	return errors.New("the macOS config disk requires a macOS host")
}
