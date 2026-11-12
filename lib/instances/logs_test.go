package instances

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShouldSkipAppLogLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "program start marker",
			line: "2026-03-10T10:00:00Z [INFO] HYPEMAN-PROGRAM-START ts=2026-03-10T10:00:00Z mode=exec",
			want: true,
		},
		{
			name: "agent ready marker",
			line: "2026-03-10T10:00:01Z [INFO] HYPEMAN-AGENT-READY ts=2026-03-10T10:00:01Z",
			want: true,
		},
		{
			name: "headers marker",
			line: "2026-03-10T10:00:02Z [INFO] HYPEMAN-HEADERS-READY",
			want: true,
		},
		{
			name: "normal app log line",
			line: "2026-03-10T10:00:03Z [INFO] build completed successfully",
			want: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, shouldSkipAppLogLine(tt.line))
		})
	}
}

func TestArchiveAppLogForBootArchivesJailedLinkTarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	m := &manager{paths: paths.New(dir)}
	const id = "archive-test"
	require.NoError(t, os.MkdirAll(m.paths.InstanceLogs(id), 0755))

	// A jailed VM's log is a link to the file firecracker writes inside its chroot.
	chrootLog := filepath.Join(dir, "chroot", "serial.log")
	require.NoError(t, os.MkdirAll(filepath.Dir(chrootLog), 0755))
	require.NoError(t, os.WriteFile(chrootLog, []byte("boot 1 output\n"), 0644))
	appLog := m.paths.InstanceAppLog(id)
	require.NoError(t, os.Symlink(chrootLog, appLog))

	require.NoError(t, m.archiveAppLogForBoot(id))

	_, err := os.Lstat(appLog)
	assert.True(t, os.IsNotExist(err), "the active log path should be free for the next boot")
	_, err = os.Stat(chrootLog)
	assert.True(t, os.IsNotExist(err), "the chroot file moves out so the next boot starts empty")

	archived, err := filepath.Glob(appLog + ".prev.*")
	require.NoError(t, err)
	require.Len(t, archived, 1)
	data, err := os.ReadFile(archived[0])
	require.NoError(t, err)
	assert.Equal(t, "boot 1 output\n", string(data), "the archive keeps the earlier boot's output")
}
