//go:build !darwin || !arm64

package main

import (
	"context"
	"fmt"
)

func publish(context.Context, string, string, string, string) (string, string, error) {
	return "", "", fmt.Errorf("base publication requires an Apple-silicon macOS operator host")
}

func snapshot(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("base snapshot requires an Apple-silicon macOS operator host")
}
