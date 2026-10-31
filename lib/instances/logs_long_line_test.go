package instances

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestStreamInstanceLogsLongLine(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	require.NoError(t, m.ensureDirectories("long-line"))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{Id: "long-line", DataDir: p.InstanceDir("long-line")}}))

	// The odd-length prefix puts piece boundaries inside two-byte runes.
	longLine := "x" + strings.Repeat("é", 200*1024)
	content := "before\n" + longLine + "\nafter\n"
	require.NoError(t, os.WriteFile(p.InstanceAppLog("long-line"), []byte(content), 0644))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logs, err := m.streamInstanceLogs(ctx, "long-line", 10, false, LogSourceApp)
	require.NoError(t, err)

	var lines []string
	for {
		select {
		case line, ok := <-logs:
			if !ok {
				require.Equal(t, "before", lines[0])
				require.Equal(t, "after", lines[len(lines)-1])
				require.Equal(t, longLine, strings.Join(lines[1:len(lines)-1], ""))
				return
			}
			require.True(t, utf8.ValidString(line), "piece %d splits a UTF-8 sequence", len(lines))
			lines = append(lines, line)
		case <-ctx.Done():
			t.Fatalf("log stream did not finish; received %d lines", len(lines))
		}
	}
}

func TestStreamInstanceLogsExactSizeLine(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	require.NoError(t, m.ensureDirectories("exact-line"))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{Id: "exact-line", DataDir: p.InstanceDir("exact-line")}}))

	exactLine := strings.Repeat("x", maxLogLineBytes)
	content := "before\n" + exactLine + "\r\n" + exactLine + exactLine + "\nafter\n"
	require.NoError(t, os.WriteFile(p.InstanceAppLog("exact-line"), []byte(content), 0644))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logs, err := m.streamInstanceLogs(ctx, "exact-line", 10, false, LogSourceApp)
	require.NoError(t, err)

	var lines []string
	for {
		select {
		case line, ok := <-logs:
			if !ok {
				require.Equal(t, []string{"before", exactLine, exactLine, exactLine, "after"}, lines)
				return
			}
			lines = append(lines, line)
		case <-ctx.Done():
			t.Fatalf("log stream did not finish; received %d lines", len(lines))
		}
	}
}
