package instances

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseExitSentinelLine(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantOK   bool
		wantCode int
		wantMsg  string
	}{
		{
			name:     "standard log line with sentinel",
			line:     `2026-02-13T15:26:27Z [INFO] [hypeman-init:entrypoint] HYPEMAN-EXIT code=127 message="command not found"`,
			wantOK:   true,
			wantCode: 127,
			wantMsg:  "command not found",
		},
		{
			name:     "exit code 0",
			line:     `2026-02-13T15:26:27Z [INFO] [hypeman-init:entrypoint] HYPEMAN-EXIT code=0 message="success"`,
			wantOK:   true,
			wantCode: 0,
			wantMsg:  "success",
		},
		{
			name:     "SIGKILL with OOM",
			line:     `2026-02-13T15:26:27Z [INFO] [hypeman-init:entrypoint] HYPEMAN-EXIT code=137 message="killed by signal 9 (killed) - OOM"`,
			wantOK:   true,
			wantCode: 137,
			wantMsg:  "killed by signal 9 (killed) - OOM",
		},
		{
			name:     "message with escaped quotes",
			line:     `HYPEMAN-EXIT code=1 message="error: \"bad thing\""`,
			wantOK:   true,
			wantCode: 1,
			wantMsg:  `error: "bad thing"`,
		},
		{
			name:   "no sentinel",
			line:   "2026-02-13T15:26:27Z [INFO] [hypeman-init:entrypoint] app exited with code 127",
			wantOK: false,
		},
		{
			name:   "empty line",
			line:   "",
			wantOK: false,
		},
		{
			name:   "partial sentinel",
			line:   "HYPEMAN-EXIT",
			wantOK: false,
		},
		{
			name:     "sentinel without message",
			line:     "HYPEMAN-EXIT code=42",
			wantOK:   true,
			wantCode: 42,
			wantMsg:  "",
		},
		{
			name:   "invalid code",
			line:   "HYPEMAN-EXIT code=abc message=\"error\"",
			wantOK: false,
		},
		{
			name:     "line with carriage return from serial console",
			line:     "2026-02-13T15:26:27Z [INFO] [hypeman-init:entrypoint] HYPEMAN-EXIT code=0 message=\"success\"\r",
			wantOK:   true,
			wantCode: 0,
			wantMsg:  "success",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, msg, ok := parseExitSentinelLine(tc.line)
			require.Equal(t, tc.wantOK, ok, "parseExitSentinelLine(%q) ok=%v, want %v", tc.line, ok, tc.wantOK)
			if ok {
				assert.Equal(t, tc.wantCode, code, "exit code mismatch")
				assert.Equal(t, tc.wantMsg, msg, "exit message mismatch")
			}
		})
	}
}
