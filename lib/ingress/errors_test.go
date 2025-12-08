package ingress

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCaddyError(t *testing.T) {
	tests := []struct {
		name        string
		caddyError  string
		wantErr     error
		wantContain string
	}{
		{
			name:        "address already in use with port",
			caddyError:  `{"error":"loading config: loading new config: http app module: start: listening on 0.0.0.0:8080: listen tcp 0.0.0.0:8080: bind: address already in use"}`,
			wantErr:     ErrPortInUse,
			wantContain: "port 8080",
		},
		{
			name:        "address already in use different port",
			caddyError:  `listen tcp 0.0.0.0:443: bind: address already in use`,
			wantErr:     ErrPortInUse,
			wantContain: "port 443",
		},
		{
			name:        "address already in use generic",
			caddyError:  `address already in use`,
			wantErr:     ErrPortInUse,
			wantContain: "already bound",
		},
		{
			name:       "unrelated error",
			caddyError: `{"error":"some other caddy error"}`,
			wantErr:    nil,
		},
		{
			name:       "empty error",
			caddyError: "",
			wantErr:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ParseCaddyError(tc.caddyError)

			if tc.wantErr == nil {
				assert.Nil(t, err)
				return
			}

			require.NotNil(t, err)
			assert.True(t, errors.Is(err, tc.wantErr), "expected error to wrap %v, got %v", tc.wantErr, err)

			if tc.wantContain != "" {
				assert.Contains(t, err.Error(), tc.wantContain)
			}
		})
	}
}
