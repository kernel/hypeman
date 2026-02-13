package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescribeExitCode(t *testing.T) {
	tests := []struct {
		name     string
		code     int
		contains string // substring that must appear
	}{
		{
			name:     "success",
			code:     0,
			contains: "success",
		},
		{
			name:     "generic exit code",
			code:     1,
			contains: "exit code 1",
		},
		{
			name:     "permission denied",
			code:     126,
			contains: "permission denied",
		},
		{
			name:     "command not found",
			code:     127,
			contains: "command not found",
		},
		{
			name:     "SIGKILL (137)",
			code:     137,
			contains: "signal 9",
		},
		{
			name:     "SIGTERM (143)",
			code:     143,
			contains: "signal 15",
		},
		{
			name:     "SIGSEGV (139)",
			code:     139,
			contains: "signal 11",
		},
		{
			name:     "SIGHUP (129)",
			code:     129,
			contains: "signal 1",
		},
		{
			name:     "generic non-zero",
			code:     42,
			contains: "exit code 42",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := describeExitCode(tc.code)
			assert.Contains(t, result, tc.contains,
				"describeExitCode(%d) = %q should contain %q", tc.code, result, tc.contains)
		})
	}
}

func TestFormatExitSentinel(t *testing.T) {
	tests := []struct {
		name    string
		code    int
		message string
		want    string
	}{
		{
			name:    "success",
			code:    0,
			message: "success",
			want:    `HYPEMAN-EXIT code=0 message="success"`,
		},
		{
			name:    "command not found",
			code:    127,
			message: "command not found",
			want:    `HYPEMAN-EXIT code=127 message="command not found"`,
		},
		{
			name:    "SIGKILL",
			code:    137,
			message: `killed by signal 9 (killed)`,
			want:    `HYPEMAN-EXIT code=137 message="killed by signal 9 (killed)"`,
		},
		{
			name:    "message with quotes",
			code:    1,
			message: `error: "bad thing"`,
			want:    `HYPEMAN-EXIT code=1 message="error: \"bad thing\""`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := formatExitSentinel(tc.code, tc.message)
			require.Equal(t, tc.want, result)
		})
	}
}

func TestIsOOMLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "Out of memory message",
			line: "6,1234,56789,-;Out of memory: Killed process 42 (my-app) total-vm:1024kB",
			want: true,
		},
		{
			name: "oom-kill event",
			line: "6,1235,56790,-;oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=/,mems_allowed=0",
			want: true,
		},
		{
			name: "oom_reaper message",
			line: "6,1236,56791,-;oom_reaper: reaped process 1234 (my-app), now anon-rss:0kB",
			want: true,
		},
		{
			name: "normal kernel message",
			line: "6,1237,56792,-;eth0: link up, 1000Mbps, full-duplex",
			want: false,
		},
		{
			name: "empty line",
			line: "",
			want: false,
		},
		{
			name: "process killed by user (not OOM)",
			line: "6,1238,56793,-;process 42 exited with signal 9",
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isOOMLine(tc.line))
		})
	}
}
