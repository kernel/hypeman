package instances

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseExitSentinelLine(t *testing.T) {
	t.Parallel()
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

func TestParseProgramStartSentinelLine(t *testing.T) {
	t.Parallel()

	ts := "2026-03-08T15:09:26.123456789Z"
	line := "2026-03-08T15:09:26Z [INFO] [hypeman-init:entrypoint] HYPEMAN-PROGRAM-START ts=" + ts + " mode=exec"

	parsed, ok := parseProgramStartSentinelLine(line)
	require.True(t, ok)
	assert.Equal(t, ts, parsed.UTC().Format(time.RFC3339Nano))
}

func TestParseAgentReadySentinelLine(t *testing.T) {
	t.Parallel()

	ts := "2026-03-08T15:09:26.987654321Z"
	line := "2026/03/08 15:09:26 [guest-agent] HYPEMAN-AGENT-READY ts=" + ts

	parsed, ok := parseAgentReadySentinelLine(line)
	require.True(t, ok)
	assert.Equal(t, ts, parsed.UTC().Format(time.RFC3339Nano))
}

func TestDeriveRunningState(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()

	tests := []struct {
		name   string
		stored StoredMetadata
		want   State
	}{
		{
			name: "initializing when program start marker missing",
			stored: StoredMetadata{
				SkipGuestAgent: false,
			},
			want: StateInitializing,
		},
		{
			name: "initializing when guest-agent marker missing",
			stored: StoredMetadata{
				ProgramStartedAt: &now,
				SkipGuestAgent:   false,
			},
			want: StateInitializing,
		},
		{
			name: "running when both markers present",
			stored: StoredMetadata{
				ProgramStartedAt:  &now,
				GuestAgentReadyAt: &now,
				SkipGuestAgent:    false,
			},
			want: StateRunning,
		},
		{
			name: "running when guest-agent is skipped",
			stored: StoredMetadata{
				ProgramStartedAt: &now,
				SkipGuestAgent:   true,
			},
			want: StateRunning,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, deriveRunningState(&tt.stored))
		})
	}
}
