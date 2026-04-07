//go:build !linux

package autostandby

import (
	"context"
	"fmt"
)

// ConntrackSource is unavailable on non-Linux platforms.
type ConntrackSource struct{}

// NewConntrackSource creates an unsupported conntrack source.
func NewConntrackSource() *ConntrackSource {
	return &ConntrackSource{}
}

// ListConnections reports that conntrack-backed auto-standby is unsupported.
func (*ConntrackSource) ListConnections(context.Context) ([]Connection, error) {
	return nil, fmt.Errorf("conntrack-backed auto-standby is only supported on Linux")
}

// OpenStream reports that conntrack-backed auto-standby is unsupported.
func (*ConntrackSource) OpenStream(context.Context) (ConnectionStream, error) {
	return nil, fmt.Errorf("conntrack-backed auto-standby is only supported on Linux")
}
