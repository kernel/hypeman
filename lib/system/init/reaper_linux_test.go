package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestOrphanReaperIntegration(t *testing.T) {
	if os.Getenv("HYPEMAN_REAPER_HELPER") == "1" {
		runOrphanReaperHelper(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestOrphanReaperIntegration$")
	cmd.Env = append(os.Environ(), "HYPEMAN_REAPER_HELPER=1")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func runOrphanReaperHelper(t *testing.T) {
	require.NoError(t, unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0))

	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	appCmd := exec.Command("/bin/sh", "-c", `sleep 0.05 & echo $! > "$1"`, "sh", pidFile)
	require.NoError(t, appCmd.Start())

	stopReaper := startOrphanReaper(map[int]struct{}{appCmd.Process.Pid: {}})
	defer stopReaper()
	require.NoError(t, appCmd.Wait())

	pidBytes, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	grandchildPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	require.NoError(t, err)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", grandchildPID)); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("adopted child %d was not reaped", grandchildPID)
}
