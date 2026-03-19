package instances

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
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

func TestHydrateBootMarkersFromLogs_RescanThrottle(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	m := &manager{
		paths: paths.New(tmpDir),
	}

	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	meta := &StoredMetadata{
		Id:             "test-instance",
		SkipGuestAgent: false,
	}

	// First call finds nothing and schedules a deferred rescan.
	hydrated := m.hydrateBootMarkersFromLogs(meta)
	require.False(t, hydrated)
	require.Nil(t, meta.ProgramStartedAt)
	require.Nil(t, meta.GuestAgentReadyAt)

	logPath := m.paths.InstanceAppLog(meta.Id)
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o755))
	err := os.WriteFile(logPath, []byte(
		"HYPEMAN-AGENT-READY ts=2026-03-08T12:00:00Z\n"+
			"HYPEMAN-PROGRAM-START ts=2026-03-08T12:00:01Z mode=exec\n",
	), 0o644)
	require.NoError(t, err)

	// Immediate second call should be throttled and skip scanning.
	hydrated = m.hydrateBootMarkersFromLogs(meta)
	require.False(t, hydrated)
	require.Nil(t, meta.ProgramStartedAt)
	require.Nil(t, meta.GuestAgentReadyAt)

	// Once the rescan interval has elapsed, markers are hydrated.
	now = now.Add(bootMarkerRescanInterval + time.Millisecond)
	hydrated = m.hydrateBootMarkersFromLogs(meta)
	require.True(t, hydrated)
	require.NotNil(t, meta.ProgramStartedAt)
	require.NotNil(t, meta.GuestAgentReadyAt)
}

func TestParseBootMarkers_IgnoresStaleMarkersBeforeBootStart(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	m := &manager{
		paths: paths.New(tmpDir),
	}

	id := "boot-markers-instance"
	logPath := m.paths.InstanceAppLog(id)
	rotatedLogPath := logPath + ".1"
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o755))

	bootStart := time.Date(2026, 3, 9, 4, 0, 0, 0, time.UTC)
	staleProgram := bootStart.Add(-2 * time.Minute)
	staleAgent := bootStart.Add(-90 * time.Second)
	freshProgram := bootStart.Add(2 * time.Second)
	freshAgent := bootStart.Add(3 * time.Second)

	staleData := "" +
		"HYPEMAN-PROGRAM-START ts=" + staleProgram.Format(time.RFC3339Nano) + " mode=exec\n" +
		"HYPEMAN-AGENT-READY ts=" + staleAgent.Format(time.RFC3339Nano) + "\n"
	require.NoError(t, os.WriteFile(rotatedLogPath, []byte(staleData), 0o644))
	require.NoError(t, os.Chtimes(rotatedLogPath, bootStart.Add(-time.Minute), bootStart.Add(-time.Minute)))

	freshData := "" +
		"HYPEMAN-PROGRAM-START ts=" + freshProgram.Format(time.RFC3339Nano) + " mode=exec\n" +
		"HYPEMAN-AGENT-READY ts=" + freshAgent.Format(time.RFC3339Nano) + "\n"
	require.NoError(t, os.WriteFile(logPath, []byte(freshData), 0o644))
	require.NoError(t, os.Chtimes(logPath, bootStart.Add(time.Second), bootStart.Add(time.Second)))

	programStartedAt, guestAgentReadyAt := m.parseBootMarkers(id, true, true, &bootStart)
	require.NotNil(t, programStartedAt)
	require.NotNil(t, guestAgentReadyAt)
	assert.Equal(t, freshProgram.Format(time.RFC3339Nano), programStartedAt.UTC().Format(time.RFC3339Nano))
	assert.Equal(t, freshAgent.Format(time.RFC3339Nano), guestAgentReadyAt.UTC().Format(time.RFC3339Nano))
}

func TestParseBootMarkers_ReturnsLatestMarkerFromNewestLog(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	m := &manager{
		paths: paths.New(tmpDir),
	}

	id := "latest-marker-instance"
	logPath := m.paths.InstanceAppLog(id)
	rotatedLogPath := logPath + ".1"
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o755))

	oldProgram := time.Date(2026, 3, 9, 4, 0, 0, 0, time.UTC)
	oldAgent := oldProgram.Add(500 * time.Millisecond)
	newProgram := oldProgram.Add(3 * time.Second)
	newProgramLatest := oldProgram.Add(4 * time.Second)
	newAgent := oldProgram.Add(3500 * time.Millisecond)

	require.NoError(t, os.WriteFile(rotatedLogPath, []byte(
		"HYPEMAN-PROGRAM-START ts="+oldProgram.Format(time.RFC3339Nano)+" mode=exec\n"+
			"HYPEMAN-AGENT-READY ts="+oldAgent.Format(time.RFC3339Nano)+"\n",
	), 0o644))

	require.NoError(t, os.WriteFile(logPath, []byte(
		"HYPEMAN-PROGRAM-START ts="+newProgram.Format(time.RFC3339Nano)+" mode=exec\n"+
			"HYPEMAN-AGENT-READY ts="+newAgent.Format(time.RFC3339Nano)+"\n"+
			"HYPEMAN-PROGRAM-START ts="+newProgramLatest.Format(time.RFC3339Nano)+" mode=exec\n",
	), 0o644))

	programStartedAt, guestAgentReadyAt := m.parseBootMarkers(id, true, true, nil)
	require.NotNil(t, programStartedAt)
	require.NotNil(t, guestAgentReadyAt)
	assert.Equal(t, newProgramLatest.Format(time.RFC3339Nano), programStartedAt.UTC().Format(time.RFC3339Nano))
	assert.Equal(t, newAgent.Format(time.RFC3339Nano), guestAgentReadyAt.UTC().Format(time.RFC3339Nano))
}

func TestAppLogPathsForMarkerScan_IgnoresArchivedLogs(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	m := &manager{
		paths: paths.New(tmpDir),
	}

	id := "log-order-instance"
	logPath := m.paths.InstanceAppLog(id)
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o755))

	for _, p := range []string{
		logPath,
		logPath + ".1",
		logPath + ".2",
		logPath + ".prev.12345",
		logPath + "-debug-copy",
	} {
		require.NoError(t, os.WriteFile(p, []byte("x\n"), 0o644))
	}

	paths := m.appLogPathsForMarkerScan(id)
	require.Equal(t, []string{logPath + ".2", logPath + ".1", logPath}, paths)
}
