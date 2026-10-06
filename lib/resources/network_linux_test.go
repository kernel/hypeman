//go:build linux

package resources

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewNetworkResourceCapacity(t *testing.T) {
	for _, tt := range []struct {
		name       string
		iface      string
		configured string
		want       int64
		warn       bool
		wantErr    bool
	}{
		{name: "missing interface", iface: "hypeman-test", want: 1_250_000_000, warn: true},
		{name: "loopback without speed", iface: "lo", want: 1_250_000_000, warn: true},
		{name: "configured capacity", iface: "hypeman-test", configured: "2Gbps", want: 250_000_000},
		{name: "invalid configured capacity", iface: "hypeman-test", configured: "invalid", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			ctx := logger.AddToContext(context.Background(), slog.New(slog.NewJSONHandler(&output, nil)))
			cfg := &config.Config{
				Capacity: config.CapacityConfig{Network: tt.configured},
				Network:  config.NetworkConfig{UplinkInterface: tt.iface},
			}
			network, err := NewNetworkResource(ctx, cfg, nil)
			if tt.wantErr {
				require.ErrorContains(t, err, "parse network limit")
				assert.Nil(t, network)
				assert.Empty(t, output.String())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, network.Capacity())
			if tt.warn {
				var record struct {
					Level     string
					Msg       string
					Interface string
				}
				require.NoError(t, json.Unmarshal(output.Bytes(), &record))
				assert.Equal(t, "WARN", record.Level)
				assert.Contains(t, record.Msg, "falling back to 10Gbps")
				assert.Equal(t, tt.iface, record.Interface)
			} else {
				assert.Empty(t, output.String())
			}
		})
	}
}
