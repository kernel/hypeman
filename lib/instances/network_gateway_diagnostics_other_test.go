//go:build !linux

package instances

import (
	"context"
	"testing"
)

func logGatewayDiagnostics(_ context.Context, _ *testing.T, _ *Instance, _, _, _ string, _ int) {
}
